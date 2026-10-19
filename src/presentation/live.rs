//! Ratatui widgets render a bounded frame; native painting has no input reader.

use super::model::{Model, TaskPhase, TaskRow, clip, sanitize_text, source_label};
use crate::{events::TaskState, timing::format_elapsed};
use ratatui::{
    buffer::Buffer,
    layout::Rect,
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::{Block, Borders, Gauge, Paragraph, Widget},
};
use std::time::Duration;

pub struct FrameInfo<'a> {
    pub command: &'a str,
    pub started: &'a str,
    pub operation: &'a str,
    pub log: Option<&'a str>,
    pub elapsed: Duration,
    pub color: bool,
    pub max_tasks: usize,
}

pub fn render(model: &Model, info: &FrameInfo<'_>, width: u16, height: u16) -> Buffer {
    let area = Rect::new(0, 0, width, height);
    let mut buffer = Buffer::empty(area);
    if width == 0 || height == 0 {
        return buffer;
    }
    let heading = if info.color {
        Style::default().add_modifier(Modifier::BOLD)
    } else {
        Style::default()
    };
    let header_count = usize::from(height.saturating_sub(1)).min(3);
    let tail_minimum = if height >= 8 { 3 } else { 0 };
    let task_count = info
        .max_tasks
        .min(model.rows().len())
        .min(usize::from(height).saturating_sub(header_count + tail_minimum));
    let tail_height = usize::from(height).saturating_sub(header_count + task_count);
    let operation = format!("Operation: {}", single_line(info.operation));
    let operation = if let Some(log) = info.log {
        let complete = format!("{operation}  Log: {}", single_line(log));
        if Span::raw(&complete).width() <= usize::from(width) {
            complete
        } else {
            let omitted = format!("{operation}  Log: …");
            if Span::raw(&omitted).width() <= usize::from(width) {
                omitted
            } else {
                "Log: …".into()
            }
        }
    } else {
        operation
    };
    let headers = [
        format!("Command: {}", info.command),
        format!(
            "Started: {}  Elapsed: {}",
            info.started,
            format_elapsed(info.elapsed)
        ),
        operation,
    ];
    for (index, text) in headers.iter().take(header_count).enumerate() {
        Line::styled(clipped(&single_line(text), usize::from(width)), heading)
            .render(Rect::new(0, index as u16, width, 1), &mut buffer);
    }
    if tail_height > 0 {
        let y = header_count as u16;
        let tail_area = Rect::new(0, y, width, tail_height as u16);
        let bordered = tail_height >= 3 && width >= 3;
        let inner_height = tail_height.saturating_sub(if bordered { 2 } else { 0 });
        let mut lines: Vec<_> = model.tail().iter().map(|line| line.as_str()).collect();
        if !model.current_tail_line().is_empty() {
            lines.push(model.current_tail_line());
        }
        let start = lines.len().saturating_sub(inner_height);
        let lines: Vec<Line<'_>> = lines[start..]
            .iter()
            .map(|line| {
                Line::raw(tail_line(
                    line,
                    usize::from(width.saturating_sub(if bordered { 2 } else { 0 })),
                ))
            })
            .collect();
        let text = Paragraph::new(lines);
        if bordered {
            text.block(
                Block::default()
                    .borders(Borders::ALL)
                    .title("Output")
                    .border_style(heading),
            )
            .render(tail_area, &mut buffer);
        } else {
            text.render(tail_area, &mut buffer);
        }
    }
    for (index, row) in model.visible_rows(task_count).iter().enumerate() {
        let y = header_count + tail_height + index;
        let row_area = Rect::new(0, y as u16, width, 1);
        let (label, _) = task_label(row, usize::from(width));
        let color = match row.phase {
            TaskPhase::Final {
                state: Some(TaskState::Succeeded),
                ..
            } => Color::Green,
            TaskPhase::Final {
                state: Some(TaskState::Failed),
                ..
            } => Color::Red,
            TaskPhase::Final { .. } | TaskPhase::Pending => Color::Yellow,
            TaskPhase::Executing => Color::Cyan,
        };
        let style = if info.color {
            Style::default().fg(color)
        } else {
            Style::default()
        };
        if let Some(fraction) = row.fraction().filter(|_| width >= 40) {
            Gauge::default()
                .ratio(fraction)
                .label(clip(&label, usize::from(width)))
                .gauge_style(style)
                .render(row_area, &mut buffer);
        } else {
            Line::from(Span::styled(clip(&label, usize::from(width)), style))
                .render(row_area, &mut buffer);
        }
    }
    buffer
}

fn clipped(text: &str, columns: usize) -> String {
    if Span::raw(text).width() <= columns {
        text.to_owned()
    } else if columns == 0 {
        String::new()
    } else {
        format!("{}…", clip(text, columns - 1))
    }
}

fn tail_line(line: &str, columns: usize) -> String {
    // Split only the prefix made by Tail, never interpret child text as an ID.
    let Some((source, content)) = line.split_once(' ') else {
        return clipped(line, columns);
    };
    let message_space = Span::raw(content).width().min(6);
    if source.len() + 1 + message_space <= columns {
        format!("{source} {}", clipped(content, columns - source.len() - 1))
    } else {
        // No partial source key: final task/log summaries retain the association.
        clipped(&format!("… {content}"), columns)
    }
}

fn task_counts(row: &TaskRow) -> String {
    row.total
        .map(|total| format!("{}/{total}", row.completed))
        .unwrap_or_else(|| row.completed.to_string())
}

fn task_description(row: &TaskRow) -> String {
    format!(
        "{}{}",
        single_line(&row.description),
        if row.detail.is_empty() {
            String::new()
        } else {
            format!(" {}", single_line(&row.detail))
        }
    )
}

/// The boolean marks a shortened view, which needs a full final task summary.
pub(super) fn task_label(row: &TaskRow, columns: usize) -> (String, bool) {
    let state = phase_label(&row.phase);
    let source = source_label(row.id);
    let description = task_description(row);
    let prefix = format!("{state} {source} {} ", task_counts(row));
    let complete = format!("{prefix}{description}");
    if Span::raw(&complete).width() <= columns {
        return (complete, false);
    }
    let message_space = Span::raw(&description).width().min(6);
    if prefix.len() + message_space <= columns {
        return (
            format!("{prefix}{}", clipped(&description, columns - prefix.len())),
            true,
        );
    }
    let prefix = format!("{state} {source} ");
    if prefix.len() + message_space.max(1) <= columns {
        return (
            format!(
                "{prefix}{}…",
                clip(&description, columns - prefix.len() - 1)
            ),
            true,
        );
    }
    (clipped(&format!("{state} … {description}"), columns), true)
}

pub(super) fn task_summary(row: &TaskRow) -> String {
    format!(
        "{} (task {}): {}: {} {}\n",
        source_label(row.id),
        row.id.0,
        task_description(row),
        phase_label(&row.phase),
        task_counts(row)
    )
}

fn phase_label(phase: &TaskPhase) -> &'static str {
    match phase {
        TaskPhase::Pending => "pending",
        TaskPhase::Executing => "running",
        TaskPhase::Final {
            state: None,
            entered: false,
        } => "incomplete (last pending)",
        TaskPhase::Final {
            state: None,
            entered: true,
        } => "incomplete (last running)",
        TaskPhase::Final {
            state: Some(state),
            entered: true,
        } => state_label(*state),
        TaskPhase::Final {
            state: Some(TaskState::Skipped),
            entered: false,
        } => "skipped (not started)",
        TaskPhase::Final {
            state: Some(TaskState::Canceled),
            entered: false,
        } => "canceled (not started)",
        TaskPhase::Final {
            state: Some(TaskState::Failed),
            entered: false,
        } => "failed (not started)",
        TaskPhase::Final {
            state: Some(TaskState::Incomplete),
            entered: false,
        } => "incomplete (not started)",
        // apply() rejects this transition; keep the rendering honest if constructed directly.
        TaskPhase::Final {
            state: Some(TaskState::Succeeded),
            entered: false,
        } => "invalid unstarted success",
    }
}

fn single_line(text: &str) -> String {
    sanitize_text(text).replace(['\n', '\r'], " ")
}

pub fn state_label(state: TaskState) -> &'static str {
    match state {
        TaskState::Succeeded => "succeeded",
        TaskState::Failed => "failed",
        TaskState::Skipped => "skipped",
        TaskState::Incomplete => "incomplete",
        TaskState::Canceled => "canceled",
    }
}

/// Output-only inline viewport. Ratatui's stock Crossterm inline constructor
/// queries cursor position through a raw input reader; this painter instead
/// establishes a line anchor using owned output and reserves a spare column.
/// Resize/handoff invalidates geometry; it never moves into unrelated history.
pub struct RelativePainter<W: std::io::Write> {
    writer: W,
    rows: Option<u16>,
    color: bool,
    fresh_line: bool,
    hidden: bool,
}

impl<W: std::io::Write> RelativePainter<W> {
    pub fn new(writer: W, color: bool) -> Self {
        Self {
            writer,
            rows: None,
            color,
            fresh_line: false,
            hidden: false,
        }
    }
    pub fn writer_mut(&mut self) -> &mut W {
        &mut self.writer
    }
    pub fn invalidate(&mut self) {
        self.rows = None;
        self.fresh_line = false;
    }

    pub fn paint(&mut self, buffer: &Buffer) -> std::io::Result<()> {
        use crossterm::{
            QueueableCommand,
            cursor::{Hide, MoveDown, MoveToColumn, MoveUp},
            terminal::{Clear, ClearType},
        };
        let height = buffer.area.height;
        if height == 0 || buffer.area.width == 0 {
            return Ok(());
        }
        self.writer.queue(Hide)?;
        self.hidden = true;
        self.writer.queue(MoveToColumn(0))?;
        let previous = self.rows.unwrap_or(height);
        if let Some(rows) = self.rows {
            if rows > 1 {
                self.writer.queue(MoveUp(rows - 1))?;
            }
        } else {
            if !self.fresh_line {
                self.writer.write_all(b"\n")?;
            }
            for _ in 1..height {
                self.writer.write_all(b"\n")?;
            }
            if height > 1 {
                self.writer.queue(MoveUp(height - 1))?;
            }
        }
        let total = previous.max(height);
        for y in 0..total {
            self.writer
                .queue(MoveToColumn(0))?
                .queue(Clear(ClearType::CurrentLine))?;
            if y < height {
                self.write_row(buffer, y)?;
            }
            if y + 1 < total {
                if self.rows.is_none() || y + 1 >= previous {
                    self.writer.write_all(b"\n")?;
                } else {
                    self.writer.queue(MoveDown(1))?;
                }
            }
        }
        if total > height {
            self.writer.queue(MoveUp(total - height))?;
        }
        self.writer.flush()?;
        self.rows = Some(height);
        self.fresh_line = false;
        Ok(())
    }

    fn write_row(&mut self, buffer: &Buffer, y: u16) -> std::io::Result<()> {
        use crossterm::{
            QueueableCommand,
            style::{Attribute, SetAttribute, SetBackgroundColor, SetForegroundColor},
        };
        let mut x = 0;
        let mut prior = None;
        while x < buffer.area.width {
            let cell = &buffer[(x, y)];
            if self.color && prior != Some((cell.fg, cell.bg, cell.modifier)) {
                self.writer
                    .queue(SetAttribute(Attribute::Reset))?
                    .queue(SetForegroundColor(terminal_color(cell.fg)))?
                    .queue(SetBackgroundColor(terminal_color(cell.bg)))?;
                for (flag, attribute) in [
                    (Modifier::BOLD, Attribute::Bold),
                    (Modifier::DIM, Attribute::Dim),
                    (Modifier::ITALIC, Attribute::Italic),
                    (Modifier::UNDERLINED, Attribute::Underlined),
                    (Modifier::REVERSED, Attribute::Reverse),
                    (Modifier::HIDDEN, Attribute::Hidden),
                    (Modifier::CROSSED_OUT, Attribute::CrossedOut),
                ] {
                    if cell.modifier.contains(flag) {
                        self.writer.queue(SetAttribute(attribute))?;
                    }
                }
                prior = Some((cell.fg, cell.bg, cell.modifier));
            }
            let symbol = cell.symbol();
            self.writer.write_all(symbol.as_bytes())?;
            x = x.saturating_add(Span::raw(symbol).width().max(1).min(u16::MAX as usize) as u16);
        }
        if self.color {
            self.writer.queue(SetAttribute(Attribute::Reset))?;
        }
        Ok(())
    }

    /// Park at the known top line so prompts/children have exclusive output.
    pub fn park(&mut self) -> std::io::Result<()> {
        use crossterm::{
            QueueableCommand,
            cursor::{MoveDown, MoveToColumn, MoveUp, Show},
            terminal::{Clear, ClearType},
        };
        if let Some(rows) = self.rows.take() {
            self.writer.queue(MoveToColumn(0))?;
            if rows > 1 {
                self.writer.queue(MoveUp(rows - 1))?;
            }
            for y in 0..rows {
                self.writer.queue(Clear(ClearType::CurrentLine))?;
                if y + 1 < rows {
                    self.writer.queue(MoveDown(1))?;
                }
            }
            if rows > 1 {
                self.writer.queue(MoveUp(rows - 1))?;
            }
            self.fresh_line = true;
        }
        self.writer.queue(Show)?;
        self.writer.flush()?;
        self.hidden = false;
        Ok(())
    }

    /// Leave the final panel in normal scrollback, then release cursor ownership.
    pub fn finish(&mut self) -> std::io::Result<()> {
        use crossterm::{
            QueueableCommand,
            cursor::{MoveToColumn, Show},
        };
        let had_rows = self.rows.take().is_some();
        if had_rows {
            self.writer.queue(MoveToColumn(0))?;
            self.writer.write_all(b"\n")?;
        }
        if self.hidden || had_rows {
            self.writer.queue(Show)?;
        }
        self.writer.flush()?;
        self.hidden = false;
        Ok(())
    }
}

impl<W: std::io::Write> Drop for RelativePainter<W> {
    fn drop(&mut self) {
        use crossterm::{QueueableCommand, cursor::Show};
        if self.hidden {
            let _ = self.writer.queue(Show);
            let _ = self.writer.flush();
        }
    }
}

fn terminal_color(color: Color) -> crossterm::style::Color {
    use crossterm::style::Color as C;
    match color {
        Color::Reset => C::Reset,
        Color::Black => C::Black,
        Color::Red => C::DarkRed,
        Color::Green => C::DarkGreen,
        Color::Yellow => C::DarkYellow,
        Color::Blue => C::DarkBlue,
        Color::Magenta => C::DarkMagenta,
        Color::Cyan => C::DarkCyan,
        Color::Gray => C::Grey,
        Color::DarkGray => C::DarkGrey,
        Color::LightRed => C::Red,
        Color::LightGreen => C::Green,
        Color::LightYellow => C::Yellow,
        Color::LightBlue => C::Blue,
        Color::LightMagenta => C::Magenta,
        Color::LightCyan => C::Cyan,
        Color::White => C::White,
        Color::Rgb(r, g, b) => C::Rgb { r, g, b },
        Color::Indexed(value) => C::AnsiValue(value),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::events::{TaskId, UiEvent};
    fn info() -> FrameInfo<'static> {
        FrameInfo {
            command: "build node",
            started: "2026-10-18T12:00:00+02:00",
            operation: "build",
            log: Some("/logs/full-log"),
            elapsed: Duration::from_secs(61),
            color: false,
            max_tasks: 5,
        }
    }
    fn text(buffer: &Buffer) -> String {
        buffer.content().iter().map(|cell| cell.symbol()).collect()
    }
    #[test]
    fn normal_frame_reserves_twelve_tail_rows_and_bounded_tasks() {
        let mut model = Model::new(20, 100);
        for id in 0..8 {
            model
                .apply(&UiEvent::TaskStarted {
                    task: TaskId(id),
                    description: format!("task{id}"),
                })
                .expect("start");
        }
        let buffer = render(&model, &info(), 80, 22);
        let output = text(&buffer);
        assert!(output.contains("Elapsed: 1m 1s"));
        assert!(output.contains("Output"));
        assert!(output.contains("task4"));
        assert!(!output.contains("task5"));
        assert_eq!(buffer.area.height, 22);
    }
    #[test]
    fn pending_and_executing_tasks_have_distinct_labels_without_native_claims() {
        let mut model = Model::new(2, 80);
        model
            .apply(&UiEvent::TaskPending {
                task: TaskId(1),
                description: "node-a".into(),
            })
            .unwrap();
        model
            .apply(&UiEvent::TaskStarted {
                task: TaskId(2),
                description: "node-b".into(),
            })
            .unwrap();
        let output = text(&render(&model, &info(), 80, 10));
        assert!(output.contains("pending @1 0 node-a"));
        assert!(output.contains("running @2 0 node-b"));
        model
            .apply(&UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Canceled,
            })
            .unwrap();
        let summary = task_summary(&model.rows()[0]);
        assert!(summary.contains("node-a: canceled (not started)"));
    }
    #[test]
    fn tiny_zero_and_narrow_sizes_never_underflow_or_invent_percentages() {
        let mut model = Model::new(2, 80);
        model
            .apply(&UiEvent::TaskStarted {
                task: TaskId(1),
                description: "界task".into(),
            })
            .expect("start");
        model
            .apply(&UiEvent::TaskProgress {
                task: TaskId(1),
                completed: 0,
                total: Some(0),
                detail: String::new(),
            })
            .expect("progress");
        for width in [0, 1, 2, 8, 39] {
            for height in [0, 1, 2, 3, 7] {
                let buffer = render(&model, &info(), width, height);
                assert_eq!(buffer.area.width, width);
                assert!(!text(&buffer).contains('%'));
            }
        }
    }

    #[test]
    fn narrow_task_rows_keep_explicit_failure_before_long_description() {
        let mut model = Model::new(0, 80);
        model
            .apply(&UiEvent::TaskStarted {
                task: TaskId(1),
                description: "very long\nunsafe\rdescription".into(),
            })
            .expect("start");
        model
            .apply(&UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Failed,
            })
            .expect("result");
        let buffer = render(&model, &info(), 12, 4);
        let output = text(&buffer);
        assert!(output.contains("failed"));
        assert!(output.contains('…'));
        assert!(!output.contains('@'), "no partial source key in a tiny row");
        assert!(!output.contains(['\n', '\r']));
    }

    #[test]
    fn tail_source_id_matches_task_description_without_color() {
        let mut model = Model::new(2, 80);
        model
            .apply(&UiEvent::TaskStarted {
                task: TaskId(7),
                description: "node-a activation".into(),
            })
            .expect("start");
        model.output(
            TaskId(7),
            crate::events::OutputStream::Stderr,
            b"failed to switch\n",
        );
        model
            .apply(&UiEvent::TaskFinished {
                task: TaskId(7),
                state: TaskState::Failed,
            })
            .expect("result");
        let output = text(&render(&model, &info(), 80, 12));
        assert!(output.contains("@7/e failed to switch"));
        assert!(output.contains("failed @7 0 node-a activation"));
    }

    #[test]
    fn narrow_two_task_frames_keep_large_keys_outcomes_and_messages_without_color() {
        let mut model = Model::new(4, 80);
        for (id, description, message) in [
            (u64::MAX - 1, "node-a activation", "failed to switch\n"),
            (u64::MAX, "node-b activation", "failed to copy\n"),
        ] {
            model
                .apply(&UiEvent::TaskStarted {
                    task: TaskId(id),
                    description: description.into(),
                })
                .expect("start");
            model.output(
                TaskId(id),
                crate::events::OutputStream::Stderr,
                message.as_bytes(),
            );
            model
                .apply(&UiEvent::TaskFinished {
                    task: TaskId(id),
                    state: TaskState::Failed,
                })
                .expect("result");
        }
        for width in [32, 39, 40] {
            let output = text(&render(&model, &info(), width, 14));
            for key in ["@3w5e11264sgse", "@3w5e11264sgsf"] {
                assert!(output.matches(key).count() >= 2, "row and tail keep {key}");
            }
            assert!(output.contains("node-a"));
            assert!(output.contains("node-b"));
            assert!(output.matches("failed to").count() >= 2);
            assert!(output.matches("failed @").count() >= 2);
        }
    }

    #[test]
    fn tiny_layout_omits_whole_keys_and_retains_full_summary_identity() {
        let mut model = Model::new(2, 80);
        model
            .apply(&UiEvent::TaskStarted {
                task: TaskId(u64::MAX),
                description: "node-a activation".into(),
            })
            .expect("start");
        model.output(
            TaskId(u64::MAX),
            crate::events::OutputStream::Stderr,
            b"failed to switch\n",
        );
        let row = &model.rows()[0];
        for columns in [0, 1, 2, 8, 12, 20] {
            let (label, shortened) = task_label(row, columns);
            assert!(shortened);
            assert!(!label.contains('@'));
            assert!(Span::raw(&label).width() <= columns);
        }
        let mut sparse = row.clone();
        sparse.description.clear();
        sparse.completed = u64::MAX;
        sparse.total = Some(u64::MAX);
        for columns in [0, 1, 14, 23, 24, 25, 26, 32, 39, 40] {
            let (label, _) = task_label(&sparse, columns);
            assert!(Span::raw(&label).width() <= columns);
            if label.contains('@') {
                assert!(label.contains("@3w5e11264sgsf"));
            }
        }
        let output = text(&render(&model, &info(), 20, 8));
        assert!(!output.contains('@'));
        assert!(output.contains('…'));
        let summary = task_summary(row);
        assert!(summary.contains("@3w5e11264sgsf (task 18446744073709551615)"));
        assert!(summary.contains("node-a activation: running"));
    }

    #[test]
    fn relative_painter_uses_output_only_anchor_and_reanchors_after_handoff() {
        let mut bytes = Vec::new();
        {
            let mut painter = RelativePainter::new(&mut bytes, false);
            let buffer = Buffer::with_lines(["first", "last"]);
            painter.paint(&buffer).expect("paint");
            painter.park().expect("park");
            painter.writer_mut().extend_from_slice(b"prompt\n");
            painter.invalidate();
            painter.paint(&buffer).expect("repaint");
            painter.finish().expect("finish");
        }
        assert!(!bytes.windows(4).any(|bytes| bytes == b"\x1b[6n"));
        assert!(!bytes.windows(8).any(|bytes| bytes == b"\x1b[?1049h"));
        assert!(bytes.windows(7).any(|bytes| bytes == b"prompt\n"));
        assert_eq!(
            bytes.windows(5).filter(|bytes| *bytes == b"first").count(),
            2
        );
    }
}
