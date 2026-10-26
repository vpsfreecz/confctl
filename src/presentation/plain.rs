//! Baseline text formatting. Character padding matches the Ruby formatter;
//! display-width clipping belongs only to the live renderer.

use crate::error::ConfctlError;
use crate::events::{Style, TableAlignment, TableFormat, TableLayout};

pub fn diagnostic(text: &str, style: Style, color: bool) -> Vec<u8> {
    let Some(code) = style_code(style) else {
        return text.as_bytes().to_vec();
    };
    if color {
        format!("\x1b[{code}m{text}\x1b[0m").into_bytes()
    } else {
        text.as_bytes().to_vec()
    }
}

pub fn styled_diagnostic(parts: &[(Style, String)], color: bool) -> Vec<u8> {
    let mut output = String::new();
    for (style, text) in parts {
        value(&mut output, text, *style, color);
    }
    output.into_bytes()
}

fn style_code(style: Style) -> Option<&'static str> {
    Some(match style {
        Style::Plain => return None,
        Style::Heading => "1",
        Style::Machine => "33",
        Style::Generation => "36",
        Style::Success => "32",
        Style::Failure => "31",
        Style::Incomplete => "33",
    })
}

fn value(output: &mut String, text: &str, style: Style, color: bool) {
    if let Some(code) = style_code(style).filter(|_| color) {
        output.push_str("\x1b[");
        output.push_str(code);
        output.push('m');
        output.push_str(text);
        output.push_str("\x1b[0m");
    } else {
        output.push_str(text);
    }
}

/// `lib/confctl/cli/output_formatter.rb` is the executable baseline. Columns
/// already contain display labels; callers do not send executable callbacks.
/// Empty/missing string cells use its default '-' placeholder.
pub fn table(columns: &[String], rows: &[Vec<String>], format: &TableFormat) -> Vec<u8> {
    styled_table(columns, rows, &[], format, false).expect("empty styles match any table")
}

pub(crate) fn validate_cell_styles(
    columns: &[String],
    rows: &[Vec<String>],
    styles: &[Vec<Style>],
) -> Result<(), ConfctlError> {
    if styles.len() > rows.len() || styles.iter().any(|row| row.len() > columns.len()) {
        return Err(ConfctlError::EventDelivery(
            "table cell styles exceed its row/column shape".into(),
        ));
    }
    Ok(())
}

pub fn styled_table(
    columns: &[String],
    rows: &[Vec<String>],
    styles: &[Vec<Style>],
    format: &TableFormat,
    color: bool,
) -> Result<Vec<u8>, ConfctlError> {
    validate_cell_styles(columns, rows, styles)?;
    if columns.is_empty() {
        return Ok(Vec::new());
    }
    let values: Vec<Vec<&str>> = rows
        .iter()
        .map(|row| {
            (0..columns.len())
                .map(|index| {
                    row.get(index)
                        .map(String::as_str)
                        .filter(|value| !value.is_empty())
                        .unwrap_or("-")
                })
                .collect()
        })
        .collect();
    let layout = match format.layout {
        TableLayout::Auto if rows.len() > 1 => TableLayout::Columns,
        TableLayout::Auto => TableLayout::Rows,
        layout => layout,
    };
    let mut output = String::new();
    match layout {
        TableLayout::Columns => {
            let widths: Vec<_> = columns
                .iter()
                .enumerate()
                .map(|(index, label)| {
                    values
                        .iter()
                        .map(|row| row[index].chars().count())
                        .chain(std::iter::once(label.chars().count()))
                        .max()
                        .unwrap_or(0)
                        + 1
                })
                .collect();
            let mut line = |cells: &[&str], styles: &[Style]| {
                for (index, text) in cells.iter().enumerate() {
                    if index > 0 {
                        output.push_str("  ");
                    }
                    let padding = if index + 1 == columns.len() {
                        0
                    } else {
                        widths[index].saturating_sub(text.chars().count())
                    };
                    let right = format.align.get(index) == Some(&TableAlignment::Right);
                    if right {
                        output.extend(std::iter::repeat_n(' ', padding));
                    }
                    value(
                        &mut output,
                        text,
                        styles.get(index).copied().unwrap_or(Style::Plain),
                        color,
                    );
                    if !right {
                        output.extend(std::iter::repeat_n(' ', padding));
                    }
                }
                output.push('\n');
            };
            if format.header {
                line(&columns.iter().map(String::as_str).collect::<Vec<_>>(), &[]);
            }
            for (index, row) in values.iter().enumerate() {
                line(row, styles.get(index).map(Vec::as_slice).unwrap_or(&[]));
            }
        }
        TableLayout::Rows => {
            let heading_width = columns
                .iter()
                .map(|label| label.chars().count())
                .max()
                .unwrap_or(0)
                + 1;
            for (row_index, row) in values.iter().enumerate() {
                for (column_index, (label, cell)) in columns.iter().zip(row).enumerate() {
                    let style = styles
                        .get(row_index)
                        .and_then(|row| row.get(column_index))
                        .copied()
                        .unwrap_or(Style::Plain);
                    if format.header {
                        output.extend(std::iter::repeat_n(
                            ' ',
                            heading_width - label.chars().count(),
                        ));
                        output.push_str(label);
                        output.push_str(":  ");
                        // Ruby String#split drops trailing empty fields.
                        let parts: Vec<_> = cell.split('\n').collect();
                        let end = parts
                            .iter()
                            .rposition(|part| !part.is_empty())
                            .map_or(1, |index| index + 1);
                        for (index, part) in parts[..end].iter().enumerate() {
                            if index > 0 {
                                output.push('\n');
                                output.extend(std::iter::repeat_n(' ', heading_width + 3));
                            }
                            value(&mut output, part, style, color);
                        }
                    } else {
                        value(&mut output, cell, style, color);
                    }
                    output.push('\n');
                }
                output.push('\n');
            }
        }
        TableLayout::Auto => unreachable!("auto layout resolved"),
    }
    Ok(output.into_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;
    fn columns() -> Vec<String> {
        vec!["NAME".into(), "ROLE".into()]
    }
    fn rows() -> Vec<Vec<String>> {
        vec![
            vec!["a".into(), "worker".into()],
            vec!["long".into(), "db".into()],
        ]
    }
    #[test]
    fn auto_layout_empty_one_many_matches_baseline() {
        assert_eq!(table(&[], &rows(), &TableFormat::default()), b"");
        assert_eq!(table(&columns(), &[], &TableFormat::default()), b"");
        assert_eq!(
            table(&columns(), &rows()[..1], &TableFormat::default()),
            b" NAME:  a\n ROLE:  worker\n\n"
        );
        assert_eq!(
            table(&columns(), &rows(), &TableFormat::default()),
            b"NAME   ROLE\na      worker\nlong   db\n"
        );
    }
    #[test]
    fn explicit_columns_headers_alignment_and_last_column_are_exact() {
        let mut format = TableFormat {
            layout: TableLayout::Columns,
            ..Default::default()
        };
        assert_eq!(table(&columns(), &[], &format), b"NAME   ROLE\n");
        format.header = false;
        format.align = vec![TableAlignment::Right, TableAlignment::Right];
        assert_eq!(
            table(&columns(), &rows(), &format),
            b"    a  worker\n long  db\n"
        );
        format.header = true;
        assert_eq!(
            table(&columns(), &rows(), &format),
            b" NAME  ROLE\n    a  worker\n long  db\n"
        );
    }
    #[test]
    fn rows_header_false_multiline_empty_and_non_ascii_use_ruby_character_counts() {
        let mut format = TableFormat {
            layout: TableLayout::Rows,
            ..Default::default()
        };
        assert_eq!(
            table(&["A".into()], &[vec!["line1\nline2\n".into()]], &format),
            b" A:  line1\n     line2\n\n"
        );
        format.header = false;
        assert_eq!(table(&columns(), &rows()[..1], &format), b"a\nworker\n\n");
        format.layout = TableLayout::Columns;
        assert_eq!(
            table(&columns(), &[vec![String::new()]], &format),
            b"-      -\n"
        );
        format.header = true;
        assert_eq!(
            table(
                &["N".into(), "V".into()],
                &[vec!["界".into(), "é".into()]],
                &format
            ),
            "N   V\n界   é\n".as_bytes()
        );
    }
    #[test]
    fn plain_is_byte_preserving_and_only_explicit_color_adds_controls() {
        assert_eq!(diagnostic("name\n", Style::Machine, false), b"name\n");
        assert_eq!(
            diagnostic("name", Style::Machine, true),
            b"\x1b[33mname\x1b[0m"
        );
        assert_eq!(
            diagnostic("raw\x1b[31m", Style::Plain, true),
            b"raw\x1b[31m"
        );
    }

    #[test]
    fn typed_cell_colors_leave_headers_padding_and_final_column_unstyled() {
        let columns = vec!["N".into(), "V".into()];
        let rows = vec![
            vec!["界".into(), "x".into()],
            vec!["long".into(), "tail".into()],
        ];
        let styles = vec![
            vec![Style::Success, Style::Generation],
            vec![Style::Failure],
        ];
        let mut format = TableFormat {
            layout: TableLayout::Columns,
            ..Default::default()
        };
        assert_eq!(
            styled_table(&columns, &rows, &styles, &format, false).expect("Never"),
            table(&columns, &rows, &format)
        );
        assert_eq!(
            styled_table(&columns, &rows, &styles, &format, true).expect("Always"),
            "N      V\n\x1b[32m界\x1b[0m      \x1b[36mx\x1b[0m\n\x1b[31mlong\x1b[0m   tail\n"
                .as_bytes()
        );
        format.align = vec![TableAlignment::Right, TableAlignment::Right];
        assert_eq!(
            styled_table(&columns, &rows, &styles, &format, true).expect("right"),
            "    N  V\n    \x1b[32m界\x1b[0m  \x1b[36mx\x1b[0m\n \x1b[31mlong\x1b[0m  tail\n"
                .as_bytes()
        );
        assert_eq!(
            styled_table(&columns, &rows, &[], &format, true).expect("omitted=Plain"),
            table(&columns, &rows, &format)
        );
    }

    #[test]
    fn styled_rows_keep_labels_and_multiline_continuation_indentation_plain() {
        let columns = vec!["A".into(), "B".into()];
        let rows = vec![vec!["é\n界\n".into(), "last".into()]];
        let format = TableFormat {
            layout: TableLayout::Rows,
            ..Default::default()
        };
        assert_eq!(
            styled_table(&columns, &rows, &[vec![Style::Success]], &format, true)
                .expect("multiline rows"),
            " A:  \x1b[32mé\x1b[0m\n     \x1b[32m界\x1b[0m\n B:  last\n\n".as_bytes()
        );
        assert_eq!(
            styled_table(&columns, &rows, &[vec![Style::Success]], &format, false).expect("Never"),
            " A:  é\n     界\n B:  last\n\n".as_bytes()
        );
    }

    #[test]
    fn mixed_diagnostic_preserves_order_spacing_and_bytes_when_uncolored() {
        let parts = vec![
            (Style::Heading, "Built generations\n".into()),
            (Style::Machine, "node: ".into()),
            (Style::Generation, "gen-界".into()),
            (Style::Plain, " (reused)\n".into()),
        ];
        assert_eq!(
            styled_diagnostic(&parts, false),
            "Built generations\nnode: gen-界 (reused)\n".as_bytes()
        );
        assert_eq!(styled_diagnostic(&parts, true), "\x1b[1mBuilt generations\n\x1b[0m\x1b[33mnode: \x1b[0m\x1b[36mgen-界\x1b[0m (reused)\n".as_bytes());
    }

    #[test]
    fn excess_cell_styles_are_rejected_even_when_color_is_disabled() {
        let columns = vec!["A".into()];
        let rows = vec![vec!["value".into()]];
        for styles in [
            vec![vec![], vec![]],
            vec![vec![Style::Plain, Style::Success]],
        ] {
            assert!(
                styled_table(&columns, &rows, &styles, &TableFormat::default(), false).is_err()
            );
        }
        assert!(styled_table(&[], &[], &[vec![]], &TableFormat::default(), true).is_err());
    }
}
