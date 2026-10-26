//! Ratatui widgets render a bounded frame; native painting has no input reader.

use super::model::{Model, TaskPhase, TaskRow, clip, sanitize_text};
use crate::{
    events::{NixActivityProgress, Style as SemanticStyle, TaskState},
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

#[derive(Clone, Copy)]
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
    styled_headers(info, width)
        .into_iter()
        .map(|line| line.to_string())
        .collect()
}

fn semantic_style(style: SemanticStyle, color: bool) -> Style {
    if !color {
        return Style::default();
    }
    match style {
        SemanticStyle::Heading => Style::default().add_modifier(Modifier::BOLD),
        SemanticStyle::Machine | SemanticStyle::Incomplete => Style::default().fg(Color::Yellow),
        SemanticStyle::Generation => Style::default().fg(Color::Cyan),
        SemanticStyle::Success => Style::default().fg(Color::Green),
        SemanticStyle::Failure => Style::default().fg(Color::Red),
        SemanticStyle::Plain => Style::default(),
    }
}

fn styled_headers(info: &FrameInfo<'_>, width: u16) -> Vec<Line<'static>> {
    let mut records = vec![
        vec![
            (SemanticStyle::Heading, "Command: ".into()),
            (SemanticStyle::Plain, single_line(info.command)),
        ],
        vec![
            (SemanticStyle::Heading, "Started: ".into()),
            (SemanticStyle::Plain, info.started.into()),
            (SemanticStyle::Heading, "  Elapsed: ".into()),
            (SemanticStyle::Plain, format_elapsed(info.elapsed)),
        ],
        vec![
            (SemanticStyle::Heading, "Operation: ".into()),
            (SemanticStyle::Plain, single_line(info.operation)),
        ],
    ];
    if let Some(log) = info.log {
        records.push(vec![
            (SemanticStyle::Heading, "Full log: ".into()),
            (SemanticStyle::Generation, log.into()),
        ]);
    }
    let mut lines = Vec::new();
    if width == 0 {
        return lines;
    }
    for parts in records {
        let mut line = Line::default();
        let mut used = 0;
        for (role, text) in parts {
            let style = semantic_style(role, info.color);
            for grapheme in Span::raw(&text).styled_graphemes(style) {
                let next = Span::raw(grapheme.symbol).width();
                if used + next > usize::from(width) && used != 0 {
                    lines.push(std::mem::take(&mut line));
                    used = 0;
                }
                if next <= usize::from(width) {
                    line.spans
                        .push(Span::styled(grapheme.symbol.to_owned(), style));
                    used += next;
                }
            }
        }
        lines.push(line);
    }
    lines
}

/// One geometry calculation owns panel, tail and whole-task row budgets.
struct Layout<'a> {
    header: Vec<String>,
    tasks: Vec<(&'a TaskRow, usize)>,
    tail_space: usize,
    available: usize,
}

fn task_selection(model: &Model, limit: usize, height: u16) -> Vec<(&TaskRow, usize)> {
    let mut remaining = status_budget(limit, height);
    let mut selected = Vec::new();
    for row in model.visible_rows(limit.min(remaining)) {
        if remaining == 0 {
            break;
        }
        let cost = if row.build.is_some() { 4 } else { 1 };
        let shown = cost.min(remaining);
        selected.push((row, shown));
        remaining -= shown;
    }
    selected
}

fn status_budget(limit: usize, height: u16) -> usize {
    // Reserve rows, not task-count multiples, before any work is registered.
    limit.min(usize::from(height).saturating_sub(1))
}

fn layout<'a>(model: &'a Model, info: &FrameInfo<'_>, width: u16, height: u16) -> Layout<'a> {
    let tasks = task_selection(model, info.max_tasks, height);
    let task_space = status_budget(info.max_tasks, height);
    let mut header = headers(info, width);
    header.truncate(usize::from(height).saturating_sub(task_space));
    let available = usize::from(height).saturating_sub(header.len() + task_space);
    Layout {
        header,
        tasks,
        tail_space: available.saturating_sub(2),
        available,
    }
}

fn preparing(model: &Model, info: &FrameInfo<'_>, height: u16) -> bool {
    model.visible_rows(1).is_empty()
        && !model.finalized()
        && status_budget(info.max_tasks, height) > 0
}

pub fn render(model: &Model, info: &FrameInfo<'_>, width: u16, height: u16) -> Buffer {
    if width == 0 || height == 0 {
        return Buffer::empty(Rect::new(0, 0, width, 0));
    }
    let layout = layout(model, info, width, height);
    let header_count = layout.header.len();
    let heading = if info.color {
        Style::default().add_modifier(Modifier::BOLD)
    } else {
        Style::default()
    };
    let mut lines: Vec<(String, Style)> = layout
        .header
        .into_iter()
        .map(|line| (line, Style::default()))
        .collect();
    if layout.available > 0 {
        let title = clipped("─ Live view ", usize::from(width));
        let title_width = Span::raw(&title).width();
        lines.push((
            format!(
                "{title}{}",
                "─".repeat(usize::from(width).saturating_sub(title_width))
            ),
            heading,
        ));
        if layout.available > 1 {
            let tail: Vec<_> = model
                .display_tail()
                .into_iter()
                .flat_map(|text| wrap(&text, usize::from(width)))
                .collect();
            let count = tail.len().min(layout.tail_space);
            for text in &tail[tail.len() - count..] {
                lines.push((text.clone(), Style::default()));
            }
            // Padding is inside the owned view from its very first quiet frame.
            for _ in count..layout.tail_space {
                lines.push((String::new(), Style::default()));
            }
            lines.push(("─".repeat(usize::from(width)), heading));
        }
    }
    for (row, shown) in layout.tasks {
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
        lines.push((task_label(row, usize::from(width)).0, style));
        if let Some((building, fetching)) = row.build {
            lines.extend(
                activity_lines(
                    building,
                    fetching,
                    usize::from(width),
                    (row.phase == TaskPhase::Executing && !model.finalized())
                        .then_some(info.elapsed),
                )
                .into_iter()
                .take(shown - 1)
                .map(|line| (line, style)),
            );
        }
    }
    if preparing(model, info, height) {
        let bar_width = usize::from(width).saturating_sub(26);
        lines.push((
            clipped(
                &format!(
                    "Preparing {} waiting --%",
                    indeterminate_bar(bar_width, Some(info.elapsed))
                ),
                usize::from(width),
            ),
            if info.color {
                Style::default().fg(Color::Cyan)
            } else {
                Style::default()
            },
        ));
    }
    let mut buffer = Buffer::empty(Rect::new(0, 0, width, height));
    for (y, (text, style)) in lines.into_iter().enumerate() {
        Line::styled(text, style).render(Rect::new(0, y as u16, width, 1), &mut buffer);
    }
    for (y, line) in styled_headers(info, width)
        .into_iter()
        .take(header_count)
        .enumerate()
    {
        line.render(Rect::new(0, y as u16, width, 1), &mut buffer);
    }
    buffer
}

fn combined(
    building: Option<NixActivityProgress>,
    fetching: Option<NixActivityProgress>,
) -> Option<NixActivityProgress> {
    let (building, fetching) = building.zip(fetching)?;
    Some(NixActivityProgress {
        done: building.done.checked_add(fetching.done)?,
        expected: building.expected.checked_add(fetching.expected)?,
        running: building.running.checked_add(fetching.running)?,
        failed: building.failed.checked_add(fetching.failed)?,
    })
}

fn percentage(count: NixActivityProgress) -> Option<u64> {
    (count.expected > 0 && count.done <= count.expected && count.failed == 0)
        .then(|| ((u128::from(count.done) * 100) / u128::from(count.expected)) as u64)
}

fn indeterminate(count: Option<NixActivityProgress>) -> bool {
    count.and_then(percentage).is_none() && count.is_none_or(|count| count.expected != 0)
}

fn counts(count: Option<NixActivityProgress>) -> String {
    count.map_or_else(
        || "waiting".into(),
        |count| format!("{} / {}", count.done, count.expected),
    )
}

fn activity_suffix(count: Option<NixActivityProgress>) -> String {
    count.map_or_else(String::new, |count| {
        format!("running {} failed {}", count.running, count.failed)
    })
}

fn activity_columns(values: &[Option<NixActivityProgress>; 3], columns: usize) -> (usize, usize) {
    // Both missing-count labels (waiting/unknown) have the same display width.
    let count_width = values
        .iter()
        .map(|count| counts(*count).len())
        .max()
        .unwrap_or(0);
    let suffix_width = values
        .iter()
        .map(|count| activity_suffix(*count).len())
        .max()
        .unwrap_or(0);
    // Labels, brackets, counts and percentage retain shared columns in all rows.
    let bar_width = columns.saturating_sub(9 + 3 + count_width + 1 + 4 + 1 + suffix_width);
    (count_width, bar_width)
}

fn activity_lines(
    building: Option<NixActivityProgress>,
    fetching: Option<NixActivityProgress>,
    columns: usize,
    animation: Option<Duration>,
) -> Vec<String> {
    let values = [building, fetching, combined(building, fetching)];
    let counts: Vec<_> = values
        .iter()
        .map(|count| {
            if count.is_none() && animation.is_none() {
                "unknown".into()
            } else {
                counts(*count)
            }
        })
        .collect();
    let suffixes: Vec<_> = values.iter().map(|count| activity_suffix(*count)).collect();
    let (count_width, bar_width) = activity_columns(&values, columns);
    ["Building", "Fetching", "Total"]
        .into_iter()
        .enumerate()
        .map(|(index, label)| {
            let count = values[index];
            let percent = count.and_then(percentage);
            let fraction = percent.map_or_else(|| " --%".into(), |value| format!("{value:>3}%"));
            let filled = count.filter(|_| percent.is_some()).map_or(0, |count| {
                ((u128::from(count.done) * bar_width as u128) / u128::from(count.expected)) as usize
            });
            let bar = if indeterminate(count) {
                indeterminate_bar(bar_width, animation)
            } else {
                format!(
                    "[{}{}]",
                    "=".repeat(filled),
                    " ".repeat(bar_width.saturating_sub(filled))
                )
            };
            clipped(
                &format!(
                    "{label:<8} {bar} {:>count_width$} {fraction} {}",
                    counts[index], suffixes[index]
                ),
                columns,
            )
        })
        .collect()
}

pub(super) fn spinner(elapsed: Duration) -> char {
    ['|', '/', '-', '\\'][(elapsed.as_millis() / 100 % 4) as usize]
}

fn indeterminate_bar(width: usize, animation: Option<Duration>) -> String {
    let mut cells = vec![' '; width];
    if let Some(elapsed) = animation
        && width > 0
    {
        cells[(width - 1) / 2] = spinner(elapsed);
    }
    format!("[{}]", cells.into_iter().collect::<String>())
}

/// Screen rows available to the newest tail entry, including wrapped lines.
pub(super) fn tail_capacity(model: &Model, info: &FrameInfo<'_>, width: u16, height: u16) -> usize {
    if width == 0 || height == 0 {
        0
    } else {
        layout(model, info, width, height).tail_space
    }
}

pub(super) fn headers_fit(model: &Model, info: &FrameInfo<'_>, width: u16, height: u16) -> bool {
    width > 0 && layout(model, info, width, height).header.len() == headers(info, width).len()
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
pub(super) fn result_style(state: TaskState) -> SemanticStyle {
    match state {
        TaskState::Succeeded => SemanticStyle::Success,
        TaskState::Failed => SemanticStyle::Failure,
        TaskState::Skipped | TaskState::Incomplete | TaskState::Canceled => {
            SemanticStyle::Incomplete
        }
    }
}

pub(super) fn summary_parts(row: &TaskRow, columns: usize) -> Vec<(SemanticStyle, String)> {
    let style = match row.phase {
        TaskPhase::Final {
            state: Some(state), ..
        } => result_style(state),
        _ => SemanticStyle::Incomplete,
    };
    let mut parts = vec![
        (SemanticStyle::Heading, task_description(row)),
        (SemanticStyle::Plain, ": ".into()),
        (style, phase_label(&row.phase).into()),
    ];
    if let Some((building, fetching)) = row.build {
        parts.push((SemanticStyle::Plain, "\n".into()));
        for line in activity_lines(building, fetching, columns.max(80), None) {
            parts.push((style, format!("{line}\n")));
        }
    } else {
        parts.push((SemanticStyle::Plain, format!(" {}\n", task_counts(row))));
    }
    parts
}

#[cfg(test)]
fn task_summary(row: &TaskRow) -> String {
    summary_parts(row, 100)
        .into_iter()
        .map(|(_, text)| text)
        .collect()
}
/// Match status-row visibility when a task also occupies three activity rows.
#[cfg(test)]
fn displayed_tasks(model: &Model, limit: usize, height: u16) -> Vec<crate::events::TaskId> {
    task_selection(model, limit, height)
        .into_iter()
        .filter(|(row, shown)| *shown == if row.build.is_some() { 4 } else { 1 })
        .map(|(row, _)| row.id)
        .collect()
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
    #[cfg(test)]
    pub(super) fn has_viewport(&self) -> bool {
        self.previous.is_some() || self.hidden
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
        self.park()
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
    fn frame_has_fixed_height_internal_padding_horizontal_separators_and_bounded_tasks() {
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
            let output = text(&render(&model, &info(), width, 18));
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
                building: Some(NixActivityProgress {
                    done: 2,
                    expected: 5,
                    running: 1,
                    failed: 0,
                }),
                fetching: None,
            })
            .unwrap();
        let long = format!(".confctl/logs/{}", "long-name-".repeat(8));
        let mut info = info();
        info.log = Some(&long);
        assert!(headers(&info, 32).concat().contains(&long));
        let buffer = render(&model, &info, 80, 24);
        let rendered = text(&buffer);
        for value in [
            "Building", "2 / 5", "40%", "Fetching", "Total", "waiting", "--%",
        ] {
            assert!(rendered.contains(value), "{rendered}");
        }
        assert_eq!(model.rows()[0].phase, TaskPhase::Executing);
        assert!(!rendered.contains("100%"));
        assert!(rendered.contains("2026-10-18 12:00:00 +02:00"));
        model
            .apply(&UiEvent::BuildProgress {
                task: TaskId(7),
                building: None,
                fetching: Some(NixActivityProgress::default()),
            })
            .unwrap();
        let reset = text(&render(&model, &info, 80, 24));
        assert!(reset.contains("waiting") && reset.contains("--%"));

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
                command: crate::events::CommandId(10001),
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
                building: Some(NixActivityProgress {
                    done: 2,
                    expected: 5,
                    running: 1,
                    failed: 0,
                }),
                fetching: Some(NixActivityProgress {
                    done: 3,
                    expected: 8,
                    running: 2,
                    failed: 0,
                }),
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
            for label in [
                "Building", "2 / 5", "40%", "Fetching", "3 / 8", "37%", "Total", "5 / 13", "38%",
            ] {
                assert!(visible.contains(label), "{visible}");
            }
            assert!(visible.contains(locator));
            assert!(headers(&sample, width).concat().contains(locator));
            assert!(!visible.contains("@7"));
            assert!(visible.contains("Command: confctl build -t api"));
            assert_eq!(frame.area.height, 24);
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
    fn authoritative_progress_aligns_and_keeps_unknown_invalid_and_failed_counts_honest() {
        let building = NixActivityProgress {
            done: 12,
            expected: 20,
            running: 2,
            failed: 1,
        };
        let fetching = NixActivityProgress {
            done: 3,
            expected: 8,
            running: 1,
            failed: 0,
        };
        let lines = activity_lines(Some(building), Some(fetching), 100, Some(Duration::ZERO));
        for marker in ["[", "]", "%", "running"] {
            let positions: Vec<_> = lines
                .iter()
                .map(|line| Span::raw(&line[..line.find(marker).unwrap()]).width())
                .collect();
            assert!(positions.iter().all(|position| *position == positions[0]));
        }
        assert!(lines[0].contains("--%") && lines[0].contains("running 2 failed 1"));
        assert!(lines[1].contains("37%"));
        assert!(lines[2].contains("15 / 28") && lines[2].contains("--%"));
        let grown = NixActivityProgress {
            expected: 40,
            ..building
        };
        assert_eq!(percentage(grown), None);
        assert_eq!(
            percentage(NixActivityProgress { failed: 0, ..grown }),
            Some(30)
        );
        for invalid in [
            NixActivityProgress::default(),
            NixActivityProgress {
                done: 9,
                expected: 8,
                running: 0,
                failed: 0,
            },
            NixActivityProgress {
                done: 7,
                expected: 8,
                running: 1,
                failed: 1,
            },
        ] {
            let lines = activity_lines(Some(invalid), None, 100, Some(Duration::ZERO));
            assert!(lines[0].contains("--%"));
            assert!(lines[1].contains("waiting") && lines[2].contains("waiting"));
            assert!(
                lines
                    .iter()
                    .all(|line| !line.contains("NaN") && !line.contains("100%"))
            );
        }
        let huge = NixActivityProgress {
            done: 0,
            expected: u64::MAX,
            running: 0,
            failed: 0,
        };
        assert!(combined(Some(huge), Some(huge)).is_none());
        let failed = NixActivityProgress {
            done: 2,
            expected: 2,
            running: 0,
            failed: 1,
        };
        let lines = activity_lines(
            Some(failed),
            Some(NixActivityProgress::default()),
            100,
            None,
        );
        assert!(lines[0].contains("2 / 2") && lines[0].contains("failed 1"));
        assert!(lines[2].contains("2 / 2") && lines[2].contains("failed 1"));
        assert!(
            lines
                .iter()
                .all(|line| line.contains("--%") && !line.contains("100%"))
        );
        // Running/failed are independent observations, not extra completed work.
        let independent = NixActivityProgress {
            done: u64::MAX,
            expected: u64::MAX,
            running: u64::MAX,
            failed: 0,
        };
        assert_eq!(percentage(independent), Some(100));
        assert_eq!(
            combined(Some(independent), Some(NixActivityProgress::default())),
            Some(independent)
        );
        assert!(
            combined(
                Some(independent),
                Some(NixActivityProgress {
                    failed: u64::MAX,
                    ..failed
                })
            )
            .is_none()
        );
    }

    #[test]
    fn indeterminate_spinner_is_ascii_stationary_and_fixed_at_ten_hz() {
        for width in [0, 1, 2, 3, 8, 80, 120] {
            for (millis, glyph) in [
                (0, '|'),
                (33, '|'),
                (66, '|'),
                (100, '/'),
                (200, '-'),
                (300, '\\'),
                (400, '|'),
            ] {
                let bar = indeterminate_bar(width, Some(Duration::from_millis(millis)));
                assert!(bar.is_ascii());
                assert_eq!(bar.len(), width + 2);
                if width > 0 {
                    assert_eq!(bar.chars().nth(1 + (width - 1) / 2), Some(glyph));
                }
            }
            assert_eq!(
                indeterminate_bar(width, None),
                format!("[{}]", " ".repeat(width))
            );
        }
    }

    #[test]
    fn elapsed_animates_preparation_and_unknown_builds_but_not_actual_or_terminal_counts() {
        let mut model = Model::new(12, 256);
        let mut first = info();
        first.elapsed = Duration::ZERO;
        let mut later = info();
        later.elapsed = Duration::from_millis(300);
        let preparing = render(&model, &first, 80, 24);
        assert!(text(&preparing).contains("Preparing") && model.rows().is_empty());
        assert_ne!(preparing, render(&model, &later, 80, 24));
        let monochrome_info = FrameInfo {
            elapsed: Duration::from_millis(33),
            color: false,
            ..first
        };
        let monochrome = render(&model, &monochrome_info, 80, 24);
        assert!(text(&monochrome).contains('|'));
        assert!(
            monochrome
                .content
                .iter()
                .all(|cell| cell.fg == Color::Reset)
        );
        for width in [80, 120] {
            model
                .apply(&UiEvent::TaskStarted {
                    task: TaskId(width as u64),
                    description: "build group before native spawn".into(),
                })
                .unwrap();
            model
                .apply(&UiEvent::BuildProgress {
                    task: TaskId(width as u64),
                    building: None,
                    fetching: None,
                })
                .unwrap();
            let row = model.rows().last().unwrap();
            let initial = activity_lines(None, None, width, Some(first.elapsed));
            let moved = activity_lines(None, None, width, Some(later.elapsed));
            assert_ne!(initial, moved);
            for line in &moved {
                assert!(line.contains("waiting") && line.contains("--%"));
                assert!(!line.contains('?') && !line.contains("100%"));
            }
            assert!(
                summary_parts(row, width)
                    .iter()
                    .all(|(_, line)| !line.contains('?'))
            );
            model
                .apply(&UiEvent::TaskFinished {
                    task: TaskId(width as u64),
                    state: TaskState::Failed,
                })
                .unwrap();
        }
        let frozen = text(&render(&model, &first, 80, 24));
        assert_eq!(frozen, text(&render(&model, &later, 80, 24)));
        assert!(!frozen.contains("Preparing"));
        let real = Some(NixActivityProgress {
            done: 2,
            expected: 5,
            running: 1,
            failed: 0,
        });
        assert_eq!(
            activity_lines(real, real, 80, Some(first.elapsed)),
            activity_lines(real, real, 80, Some(later.elapsed))
        );
        let zero = Some(NixActivityProgress::default());
        assert_eq!(
            activity_lines(zero, zero, 80, Some(first.elapsed)),
            activity_lines(zero, zero, 80, Some(later.elapsed))
        );
        assert!(activity_lines(zero, zero, 80, Some(first.elapsed))[0].contains("0 / 0"));
    }

    #[test]
    fn live_header_semantic_spans_keep_exact_wrapping_and_uncolored_text() {
        let mut frame_info = info();
        frame_info.color = true;
        frame_info.log = Some("\".confctl/logs/a-very-long-and-fully-retained-invocation.log\"");
        let model = Model::new(12, 256);
        for width in [40, 80, 120] {
            let colored = render(&model, &frame_info, width, 24);
            let mut plain_info = frame_info;
            plain_info.color = false;
            let plain = render(&model, &plain_info, width, 24);
            assert_eq!(text(&colored), text(&plain));
            assert_eq!(colored[(0, 0)].modifier, Modifier::BOLD);
            assert!(!colored[(9, 0)].modifier.contains(Modifier::BOLD));
            let log_rows = styled_headers(&frame_info, width);
            assert!(
                log_rows
                    .iter()
                    .flat_map(|line| &line.spans)
                    .any(|span| span.style.fg == Some(Color::Cyan))
            );
            assert_eq!(
                log_rows.iter().map(ToString::to_string).collect::<Vec<_>>(),
                headers(&plain_info, width)
            );
        }
    }

    #[test]
    fn quiet_and_busy_frames_share_geometry_and_padding_never_grows_the_panel() {
        let mut model = Model::new(100, 256);
        let quiet = render(&model, &info(), 79, 24);
        model
            .apply(&UiEvent::TaskPending {
                task: TaskId(1),
                description: "build group 1/2 (3 machines)".into(),
            })
            .unwrap();
        let pending = render(&model, &info(), 79, 24);
        model
            .apply(&UiEvent::TaskStarted {
                task: TaskId(1),
                description: "build group 1/2 (3 machines)".into(),
            })
            .unwrap();
        model
            .apply(&UiEvent::BuildProgress {
                task: TaskId(1),
                building: None,
                fetching: None,
            })
            .unwrap();
        for _ in 0..40 {
            model.output(
                TaskId(1),
                crate::events::OutputStream::Stderr,
                b"native activity\n",
            );
        }
        let busy = render(&model, &info(), 79, 24);
        model
            .apply(&UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Succeeded,
            })
            .unwrap();
        model.mark_checkpoint(&[TaskId(1)]);
        let snapshotted = render(&model, &info(), 79, 24);
        model
            .apply(&UiEvent::TaskStarted {
                task: TaskId(2),
                description: "next machine".into(),
            })
            .unwrap();
        let next_machine = render(&model, &info(), 79, 24);
        let rows = |frame: &Buffer| -> Vec<String> {
            (0..frame.area.height)
                .map(|y| {
                    (0..frame.area.width)
                        .map(|x| frame[(x, y)].symbol())
                        .collect()
                })
                .collect()
        };
        let quiet_rows = rows(&quiet);
        let separator_y = quiet_rows
            .iter()
            .position(|line| line == &"─".repeat(79))
            .unwrap();
        let status_y = separator_y + 1;
        assert_eq!(
            status_y, 19,
            "five status rows are reserved before work starts"
        );
        for frame in [&quiet, &pending, &busy, &snapshotted, &next_machine] {
            let frame_rows = rows(frame);
            assert!(frame_rows[0].starts_with("Command: confctl build -t api"));
            assert_eq!(
                frame_rows
                    .iter()
                    .position(|line| line.starts_with("─ Live view")),
                quiet_rows
                    .iter()
                    .position(|line| line.starts_with("─ Live view"))
            );
            assert_eq!(frame_rows[separator_y], "─".repeat(79));
        }
        assert!(quiet_rows[status_y].starts_with("Preparing"));
        assert!(
            quiet_rows[status_y + 1..]
                .iter()
                .all(|line| line.trim().is_empty())
        );
        assert!(rows(&pending)[status_y].starts_with("pending"));
        let busy_rows = rows(&busy);
        assert!(busy_rows[status_y].starts_with("running build group"));
        for (offset, label) in [(1, "Building"), (2, "Fetching"), (3, "Total")] {
            assert!(busy_rows[status_y + offset].starts_with(label));
            assert!(rows(&pending)[status_y + offset].trim().is_empty());
            assert!(rows(&snapshotted)[status_y + offset].trim().is_empty());
            assert!(rows(&next_machine)[status_y + offset].trim().is_empty());
        }
        assert!(rows(&next_machine)[status_y].starts_with("running 0 next machine"));
        assert_eq!(quiet.area, busy.area);
        assert_eq!(quiet.area.height, 24);
        let mut painter = RelativePainter::new(Vec::new(), false);
        painter.paint(&quiet).unwrap();
        let before = painter.writer_mut().len();
        painter.paint(&busy).unwrap();
        assert!(
            !painter.writer_mut()[before..].contains(&b'\n'),
            "tail growth must not scroll the viewport"
        );
        assert!(text(&busy).starts_with("Command: confctl build -t api"));
        assert!(
            text(&busy).contains("Building")
                && text(&busy).contains("Fetching")
                && text(&busy).contains("Total")
        );
        assert!(displayed_tasks(&model, 5, 24).contains(&TaskId(2)));
        for height in [1, 2, 4, 7, 24] {
            let frame = render(&model, &info(), 40, height);
            assert_eq!(frame.area.height, height);
            assert!(tail_capacity(&model, &info(), 40, height) <= usize::from(height));
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
