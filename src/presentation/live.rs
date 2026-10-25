//! Ratatui widgets render a bounded frame; native painting has no input reader.

use super::model::{Model, TaskPhase, TaskRow, clip, sanitize_text};
use crate::{
    events::{ActivityCount, TaskState},
    timing::format_elapsed,
};
use ratatui::{
    buffer::Buffer,
    layout::Rect,
    style::{Color, Modifier, Style},
    text::{Line, Span},
    widgets::Widget,
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

/// Wrap at display/grapheme boundaries without discarding any path bytes in
/// the sanitized display. Native PathBuf values never pass through this helper.
pub(super) fn wrap(text: &str, columns: usize) -> Vec<String> {
    if columns == 0 {
        return Vec::new();
    }
    let mut lines = Vec::new();
    let mut line = String::new();
    let mut width = 0;
    let span = Span::raw(text);
    for grapheme in span.styled_graphemes(Style::default()) {
        let next = Span::raw(grapheme.symbol).width();
        if width + next > columns && !line.is_empty() {
            lines.push(std::mem::take(&mut line));
            width = 0;
        }
        // A terminal narrower than one wide grapheme cannot display it, but
        // the full locator is also available through the scrollback fallback.
        if next <= columns {
            line.push_str(grapheme.symbol);
            width += next;
        }
    }
    if !line.is_empty() || lines.is_empty() {
        lines.push(line);
    }
    lines
}

pub(super) fn headers(info: &FrameInfo<'_>, width: u16) -> Vec<String> {
    let mut lines = Vec::new();
    for text in [
        format!("Command: {}", single_line(info.command)),
        format!(
            "Started: {}  Elapsed: {}",
            info.started,
            format_elapsed(info.elapsed)
        ),
        format!("Operation: {}", single_line(info.operation)),
    ] {
        lines.extend(wrap(&text, usize::from(width)));
    }
    if let Some(log) = info.log {
        lines.extend(wrap(&format!("Full log: {log}"), usize::from(width)));
    }
    lines
}

pub fn render(model: &Model, info: &FrameInfo<'_>, width: u16, height: u16) -> Buffer {
    if width == 0 || height == 0 {
        return Buffer::empty(Rect::new(0, 0, width, 0));
    }
    let heading = if info.color {
        Style::default().add_modifier(Modifier::BOLD)
    } else {
        Style::default()
    };
    let mut lines: Vec<(String, Style)> = headers(info, width)
        .into_iter()
        .map(|text| (text, heading))
        .collect();
    let selected = model.visible_rows(info.max_tasks.min(usize::from(height).saturating_sub(1)));
    let mut tasks = Vec::new();
    for row in selected {
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
        tasks.push((task_label(row, usize::from(width)).0, style));
        if let Some((building, fetching)) = row.build {
            let total = ActivityCount {
                observed: building.observed.saturating_add(fetching.observed),
                total: building
                    .total
                    .zip(fetching.total)
                    .and_then(|(a, b)| a.checked_add(b)),
            };
            for (label, count) in [
                ("Building", building),
                ("Fetching", fetching),
                ("Total", total),
            ] {
                tasks.push((activity_bar(label, count, usize::from(width)), style));
            }
        }
    }
    // On a tiny terminal keep outcomes first. The caller publishes the full log
    // in scrollback before the panel if its header cannot fit.
    let task_space = tasks.len().min(usize::from(height).saturating_sub(1));
    lines.truncate(usize::from(height).saturating_sub(task_space));
    let available = usize::from(height).saturating_sub(lines.len() + task_space);
    let tail: Vec<_> = model
        .display_tail()
        .into_iter()
        .flat_map(|text| wrap(&text, usize::from(width)))
        .collect();
    if available > 0 {
        let title = clipped("─ Live view ", usize::from(width));
        let title_width = Span::raw(&title).width();
        lines.push((
            format!(
                "{title}{}",
                "─".repeat(usize::from(width).saturating_sub(title_width))
            ),
            heading,
        ));
        if available > 1 {
            let count = tail.len().min(tail_capacity(model, info, width, height));
            for text in &tail[tail.len() - count..] {
                lines.push((text.clone(), Style::default()));
            }
            lines.push(("─".repeat(usize::from(width)), heading));
        }
    }
    lines.extend(tasks.into_iter().take(task_space));
    let mut buffer = Buffer::empty(Rect::new(
        0,
        0,
        width,
        lines.len().min(usize::from(height)) as u16,
    ));
    for (y, (text, style)) in lines.into_iter().take(usize::from(height)).enumerate() {
        Line::styled(text, style).render(Rect::new(0, y as u16, width, 1), &mut buffer);
    }
    buffer
}

fn activity_bar(label: &str, count: ActivityCount, columns: usize) -> String {
    let counts = count.total.map_or_else(
        || format!("{} / ?", count.observed),
        |total| format!("{} / {total}", count.observed),
    );
    let prefix = format!("{label:<8} {counts}");
    let Some(total) = count.total.filter(|n| *n > 0) else {
        return clipped(&prefix, columns);
    };
    let space = columns.saturating_sub(Span::raw(&prefix).width() + 3);
    if space < 3 {
        return clipped(&prefix, columns);
    }
    let filled =
        ((u128::from(count.observed.min(total)) * space as u128) / u128::from(total)) as usize;
    format!(
        "{prefix} [{}{}]",
        "=".repeat(filled),
        " ".repeat(space - filled)
    )
}

/// Screen rows available to the newest tail entry, including wrapped lines.
pub(super) fn tail_capacity(model: &Model, info: &FrameInfo<'_>, width: u16, height: u16) -> usize {
    if width == 0 || height == 0 {
        return 0;
    }
    let height = usize::from(height);
    let task_space = model
        .visible_rows(info.max_tasks.min(height.saturating_sub(1)))
        .iter()
        .map(|row| if row.build.is_some() { 4 } else { 1 })
        .sum::<usize>()
        .min(height.saturating_sub(1));
    let heading = headers(info, width).len().min(height - task_space);
    height.saturating_sub(heading + task_space + 2)
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
    let counts = if row.build.is_some() {
        String::new()
    } else {
        format!("{} ", task_counts(row))
    };
    let label = format!(
        "{} {counts}{}",
        phase_label(&row.phase),
        task_description(row)
    );
    let shortened = Span::raw(&label).width() > columns;
    (clipped(&label, columns), shortened)
}
pub(super) fn task_summary(row: &TaskRow) -> String {
    let mut summary = format!("{}: {}", task_description(row), phase_label(&row.phase));
    if let Some((building, fetching)) = row.build {
        let total = ActivityCount {
            observed: building.observed.saturating_add(fetching.observed),
            total: building
                .total
                .zip(fetching.total)
                .and_then(|(a, b)| a.checked_add(b)),
        };
        for (label, count) in [
            ("Building", building),
            ("Fetching", fetching),
            ("Total", total),
        ] {
            let total = count.total.map_or_else(|| "?".into(), |n| n.to_string());
            summary.push_str(&format!("; {label} {}/{total}", count.observed));
        }
    } else {
        summary.push_str(&format!(" {}", task_counts(row)));
    }
    summary.push('\n');
    summary
}
/// Match status-row visibility when a task also occupies three activity rows.
pub(super) fn displayed_tasks(
    model: &Model,
    limit: usize,
    height: u16,
) -> Vec<crate::events::TaskId> {
    let budget = usize::from(height).saturating_sub(1);
    let mut used = 0;
    let mut ids = Vec::new();
    for row in model.visible_rows(limit.min(budget)) {
        if used >= budget {
            break;
        }
        let cost = if row.build.is_some() { 4 } else { 1 };
        if used + cost <= budget {
            ids.push(row.id);
        }
        used += cost;
    }
    ids
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
    previous: Option<Buffer>,
    color: bool,
    hidden: bool,
    unfinished_line: bool,
}
impl<W: std::io::Write> RelativePainter<W> {
    pub fn new(writer: W, color: bool) -> Self {
        Self {
            writer,
            previous: None,
            color,
            hidden: false,
            unfinished_line: false,
        }
    }
    pub fn writer_mut(&mut self) -> &mut W {
        &mut self.writer
    }
    pub fn invalidate(&mut self) {
        self.previous = None;
    }
    pub(super) fn output_ended(&mut self, byte: u8) {
        self.invalidate();
        self.unfinished_line = byte != b'\n';
    }
    pub(super) fn foreign_output(&mut self) {
        self.invalidate();
        // Prompt echo and foreground children can leave the cursor midline.
        // Their bytes do not pass through this output-only painter.
        self.unfinished_line = true;
    }
    pub fn paint(&mut self, buffer: &Buffer) -> std::io::Result<()> {
        use crossterm::{
            QueueableCommand,
            cursor::{Hide, MoveToColumn},
            terminal::{Clear, ClearType},
        };
        if buffer.area.height == 0
            || buffer.area.width == 0
            || self.previous.as_ref() == Some(buffer)
        {
            return Ok(());
        }
        let height = buffer.area.height;
        let old_height = self.previous.as_ref().map_or(0, |p| p.area.height);
        let mut batch = Vec::new();
        if !self.hidden {
            batch.queue(Hide)?;
        }
        let mut at = old_height.saturating_sub(1);
        if self.previous.is_none() {
            if self.unfinished_line {
                batch.extend_from_slice(b"\n");
            }
            // The current line is the first owned line, not a blank top margin.
            batch.queue(MoveToColumn(0))?;
            for _ in 1..height {
                batch.extend_from_slice(b"\n");
            }
            at = height - 1;
        } else if height > old_height {
            for _ in old_height..height {
                batch.extend_from_slice(b"\n");
            }
            at = height - 1;
        }
        for y in 0..old_height.max(height) {
            let unchanged = self.previous.as_ref().is_some_and(|old| {
                old.area.width == buffer.area.width
                    && y < old.area.height
                    && y < height
                    && (0..buffer.area.width).all(|x| old[(x, y)] == buffer[(x, y)])
            });
            if unchanged {
                continue;
            }
            move_row(&mut batch, &mut at, y)?;
            batch
                .queue(MoveToColumn(0))?
                .queue(Clear(ClearType::CurrentLine))?;
            if y < height {
                write_row(&mut batch, buffer, y, self.color)?;
            }
        }
        move_row(&mut batch, &mut at, height - 1)?;
        self.hidden = true;
        self.writer.write_all(&batch)?;
        self.writer.flush()?;
        self.previous = Some(buffer.clone());
        self.unfinished_line = false;
        Ok(())
    }
    pub fn park(&mut self) -> std::io::Result<()> {
        use crossterm::{
            QueueableCommand,
            cursor::{MoveToColumn, Show},
            terminal::{Clear, ClearType},
        };
        if self.previous.is_none() && !self.hidden {
            return Ok(());
        }
        let mut batch = Vec::new();
        if let Some(previous) = &self.previous {
            let mut at = previous.area.height.saturating_sub(1);
            for y in 0..previous.area.height {
                move_row(&mut batch, &mut at, y)?;
                batch
                    .queue(MoveToColumn(0))?
                    .queue(Clear(ClearType::CurrentLine))?;
            }
            move_row(&mut batch, &mut at, 0)?;
        }
        batch.queue(MoveToColumn(0))?.queue(Show)?;
        self.writer.write_all(&batch)?;
        self.writer.flush()?;
        self.previous = None;
        self.hidden = false;
        Ok(())
    }
    pub fn finish(&mut self) -> std::io::Result<()> {
        use crossterm::{
            QueueableCommand,
            cursor::{MoveToColumn, Show},
        };
        if self.previous.is_none() && !self.hidden {
            return Ok(());
        }
        let mut batch = Vec::new();
        if self.previous.is_some() {
            batch.queue(MoveToColumn(0))?;
            batch.extend_from_slice(b"\n");
        }
        batch.queue(Show)?;
        self.writer.write_all(&batch)?;
        self.writer.flush()?;
        self.previous = None;
        self.hidden = false;
        Ok(())
    }
}
fn move_row(batch: &mut Vec<u8>, at: &mut u16, to: u16) -> std::io::Result<()> {
    use crossterm::{
        QueueableCommand,
        cursor::{MoveDown, MoveUp},
    };
    if to > *at {
        batch.queue(MoveDown(to - *at))?;
    } else if to < *at {
        batch.queue(MoveUp(*at - to))?;
    }
    *at = to;
    Ok(())
}
fn write_row(batch: &mut Vec<u8>, buffer: &Buffer, y: u16, color: bool) -> std::io::Result<()> {
    use crossterm::{
        QueueableCommand,
        style::{Attribute, SetAttribute, SetBackgroundColor, SetForegroundColor},
    };
    let mut x = 0;
    let mut prior = None;
    while x < buffer.area.width {
        let cell = &buffer[(x, y)];
        if color && prior != Some((cell.fg, cell.bg, cell.modifier)) {
            batch
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
                    batch.queue(SetAttribute(attribute))?;
                }
            }
            prior = Some((cell.fg, cell.bg, cell.modifier));
        }
        batch.extend_from_slice(cell.symbol().as_bytes());
        x = x.saturating_add(
            Span::raw(cell.symbol())
                .width()
                .max(1)
                .min(u16::MAX as usize) as u16,
        );
    }
    if color {
        batch.queue(SetAttribute(Attribute::Reset))?;
    }
    Ok(())
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
            command: "confctl build -t api",
            started: "2026-10-18 12:00:00 +02:00",
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
    fn frame_uses_only_content_height_horizontal_separators_and_bounded_tasks() {
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
        assert!(output.contains("─"));
        assert!(!output.contains(['│', '┌', '┐']));
        assert!(output.contains("task4"));
        assert!(!output.contains("task5"));
        assert!(buffer.area.height < 22);
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
        assert!(output.contains("pending 0 node-a"));
        assert!(output.contains("running 0 node-b"));
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
    fn single_source_is_clean_and_keeps_task_description_without_color() {
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
        assert!(output.contains("failed to switch"));
        assert!(!output.contains("@7"));
        assert!(output.contains("failed 0 node-a activation"));
    }

    #[test]
    fn narrow_mixed_sources_use_readable_labels_and_hide_internal_ids() {
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
            assert!(!output.contains("@3w5e11264sg"));
            assert!(output.contains("node-a"));
            assert!(output.contains("node-b"));
            assert!(output.matches("failed to").count() >= 2);
            assert!(output.matches("failed 0").count() >= 2);
        }
    }

    #[test]
    fn tiny_layout_keeps_full_readable_summary_without_internal_ids() {
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
        assert!(!summary.contains("18446744073709551615"));
        assert!(summary.contains("node-a activation: running"));
    }

    #[test]
    fn full_log_wraps_and_build_activity_has_three_honest_rows() {
        let mut model = Model::new(2, 256);
        model
            .apply(&UiEvent::TaskStarted {
                task: TaskId(7),
                description: "Build group 1/2 (3 machines)".into(),
            })
            .unwrap();
        model
            .apply(&UiEvent::BuildProgress {
                task: TaskId(7),
                building: ActivityCount {
                    observed: 2,
                    total: Some(5),
                },
                fetching: ActivityCount {
                    observed: 3,
                    total: None,
                },
            })
            .unwrap();
        let long = format!(".confctl/logs/{}", "long-name-".repeat(8));
        let mut info = info();
        info.log = Some(&long);
        assert!(headers(&info, 32).concat().contains(&long));
        let buffer = render(&model, &info, 80, 24);
        let rendered = text(&buffer);
        for value in ["Building 2 / 5", "Fetching 3 / ?", "Total    5 / ?"] {
            assert!(rendered.contains(value), "{rendered}");
        }
        assert_eq!(model.rows()[0].phase, TaskPhase::Executing);
        assert!(!rendered.contains('%'));
        assert!(rendered.contains("2026-10-18 12:00:00 +02:00"));
        model
            .apply(&UiEvent::BuildProgress {
                task: TaskId(7),
                building: ActivityCount {
                    observed: 0,
                    total: None,
                },
                fetching: ActivityCount {
                    observed: 0,
                    total: Some(0),
                },
            })
            .unwrap();
        let reset = text(&render(&model, &info, 80, 24));
        assert!(reset.contains("Total    0 / ?"));

        // A separate representative frame keeps the unknown/reset cases above
        // intact and exposes the actual widgets through this existing fixture.
        let mut representative = Model::new(12, 512);
        representative
            .apply(&UiEvent::TaskStarted {
                task: TaskId(7),
                description: "Build group 1/2 (3 machines)".into(),
            })
            .unwrap();
        let command = "nix build --json --no-link .#machines";
        representative
            .apply(&UiEvent::CommandStarting {
                task: TaskId(7),
                display: command.into(),
            })
            .unwrap();
        representative.apply(&UiEvent::ActivityOutput {
            task: TaskId(7), stream: crate::events::OutputStream::Stderr,
            bytes: concat!(
                "these 5 derivations will be built:\n",
                "  /nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-api-system.drv\n",
                "these 8 paths will be fetched:\n",
                "  /nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-bash-5.3\n",
                "building '/nix/store/cccccccccccccccccccccccccccccccc-api-service.drv'...\n",
                "copying path '/nix/store/dddddddddddddddddddddddddddddddd-openssl-3.6.3' from 'https://cache.nixos.org'...\n",
            ).as_bytes().to_vec(),
        }).unwrap();
        representative
            .apply(&UiEvent::BuildProgress {
                task: TaskId(7),
                building: ActivityCount {
                    observed: 2,
                    total: Some(5),
                },
                fetching: ActivityCount {
                    observed: 3,
                    total: Some(8),
                },
            })
            .unwrap();
        let locator = r#"".confctl/logs/2026-10-25_12-00-00_build_api-and-storage_6b8b038f-qualified-invocation.log""#;
        let tail = representative.display_tail();
        assert_eq!(tail.first().unwrap(), &format!("$ {command}"));
        assert!(tail[1].starts_with("these 5 derivations"));
        assert!(
            tail.iter()
                .all(|line| !line.contains("Build group 1/2 (3 machines)>"))
        );
        assert_eq!(representative.rows()[0].phase, TaskPhase::Executing);
        for width in [80, 120] {
            let sample = FrameInfo {
                log: Some(locator),
                ..info
            };
            let frame = render(&representative, &sample, width, 24);
            let visible = text(&frame);
            assert!(
                visible.find(&format!("$ {command}")).unwrap()
                    < visible.find("these 5 derivations").unwrap()
            );
            assert_eq!(visible.matches(&format!("$ {command}")).count(), 1);
            for label in ["Building 2 / 5", "Fetching 3 / 8", "Total    5 / 13"] {
                assert!(visible.contains(label), "{visible}");
            }
            assert!(visible.contains(locator));
            assert!(headers(&sample, width).concat().contains(locator));
            assert!(!visible.contains("@7"));
            assert!(visible.contains("Command: confctl build -t api"));
            assert!(frame.area.height <= 24);
            if std::env::var_os("CONFCTL_TUI_SNAPSHOT").is_some() {
                for y in 0..frame.area.height {
                    let line: String = (0..frame.area.width)
                        .map(|x| frame[(x, y)].symbol())
                        .collect();
                    println!("{}", line.trim_end());
                }
                println!();
            }
        }
    }

    #[test]
    fn painter_batches_changes_and_identical_frames_do_not_flush() {
        #[derive(Default)]
        struct Count {
            writes: usize,
            flushes: usize,
            bytes: Vec<u8>,
        }
        impl std::io::Write for Count {
            fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
                self.writes += 1;
                self.bytes.extend_from_slice(b);
                Ok(b.len())
            }
            fn flush(&mut self) -> std::io::Result<()> {
                self.flushes += 1;
                Ok(())
            }
        }
        let mut painter = RelativePainter::new(Count::default(), false);
        let buffer = Buffer::with_lines(["header", "body"]);
        painter.paint(&buffer).unwrap();
        assert_eq!(
            (painter.writer_mut().writes, painter.writer_mut().flushes),
            (1, 1)
        );
        painter.paint(&buffer).unwrap();
        assert_eq!(
            (painter.writer_mut().writes, painter.writer_mut().flushes),
            (1, 1)
        );
        let narrow = Buffer::with_lines(["tiny"]);
        painter.invalidate();
        painter.paint(&narrow).unwrap();
        assert_eq!(
            (painter.writer_mut().writes, painter.writer_mut().flushes),
            (2, 2)
        );
        painter.finish().unwrap();
    }

    #[test]
    fn fresh_anchors_preserve_unfinished_output_without_a_blank_initial_line() {
        let mut painter = RelativePainter::new(Vec::new(), false);
        let buffer = Buffer::with_lines(["view"]);
        painter.paint(&buffer).unwrap();
        assert!(!painter.writer_mut().contains(&b'\n'));
        for ending in [b"report fragment".as_slice(), b"complete report\n"] {
            painter.park().unwrap();
            painter.writer_mut().extend_from_slice(ending);
            painter.output_ended(*ending.last().unwrap());
            // Geometry changes must not discard the unfinished-line state.
            painter.invalidate();
            let start = painter.writer_mut().len();
            painter.paint(&buffer).unwrap();
            let fresh = &painter.writer_mut()[start..];
            assert_eq!(fresh.contains(&b'\n'), !ending.ends_with(b"\n"));
            if !ending.ends_with(b"\n") {
                assert!(
                    fresh.iter().position(|b| *b == b'\n').unwrap()
                        < fresh.windows(4).position(|b| b == b"\x1b[2K").unwrap()
                );
            }
        }
        painter.park().unwrap();
        painter.writer_mut().extend_from_slice(b"foreign fragment");
        painter.foreign_output();
        let start = painter.writer_mut().len();
        painter.paint(&buffer).unwrap();
        assert!(painter.writer_mut()[start..].starts_with(b"\x1b[?25l\n"));
    }

    #[test]
    fn relative_painter_uses_output_only_anchor_and_reanchors_after_handoff() {
        let mut bytes = Vec::new();
        {
            let mut painter = RelativePainter::new(&mut bytes, false);
            let buffer = Buffer::with_lines(["first", "last"]);
            painter.paint(&buffer).expect("paint");
            let first = painter.writer_mut().len();
            painter.paint(&buffer).expect("identical");
            assert_eq!(
                painter.writer_mut().len(),
                first,
                "unchanged buffer writes nothing"
            );
            let changed = Buffer::with_lines(["first", "next"]);
            painter.paint(&changed).expect("one changed row");
            let delta = &painter.writer_mut()[first..];
            assert!(!delta.windows(5).any(|w| w == b"first"));
            assert_eq!(delta.windows(4).filter(|w| *w == b"\x1b[2K").count(), 1);
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
