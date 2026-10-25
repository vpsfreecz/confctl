//! Bounded, per-native-attempt interpretation of Nix's human stderr. Raw bytes
//! always retain their independent output/capture path. Counts are activity.

use crate::events::{ActivityCount, OutputStream, TaskId, UiEvent};

const LINE_LIMIT: usize = 8192;

#[derive(Clone, Default)]
pub(crate) enum Kind {
    #[default]
    Build,
    Copy {
        host: String,
    },
}

#[derive(Default)]
pub(crate) struct Progress {
    kind: Kind,
    copy_total: Option<u64>,
    line: Vec<u8>,
    overflow: bool,
    built: u64,
    fetched: u64,
    build_total: Option<u64>,
    fetch_total: Option<u64>,
    list: Option<bool>, // true: old derivation list; false: old fetch list
}

impl Progress {
    pub(crate) fn new(kind: Kind) -> Self {
        Self {
            kind,
            ..Self::default()
        }
    }

    pub(crate) fn feed(&mut self, bytes: &[u8], task: TaskId) -> Vec<UiEvent> {
        let mut events = Vec::new();
        for byte in bytes {
            if *byte == b'\n' {
                events.extend(self.flush(task));
            } else if !self.overflow {
                if self.line.len() == LINE_LIMIT {
                    self.line.clear();
                    self.overflow = true;
                } else {
                    self.line.push(*byte);
                }
            }
        }
        events
    }

    pub(crate) fn flush(&mut self, task: TaskId) -> Vec<UiEvent> {
        let line = std::mem::take(&mut self.line);
        if std::mem::take(&mut self.overflow) {
            return Vec::new();
        }
        let Ok(line) = std::str::from_utf8(&line) else {
            return Vec::new();
        };
        self.parse(line.trim_end_matches('\r'), task)
    }

    pub(crate) fn snapshot(&self, task: TaskId) -> Option<UiEvent> {
        matches!(self.kind, Kind::Build).then(|| UiEvent::BuildProgress {
            task,
            building: ActivityCount {
                observed: self.built,
                total: self.build_total,
            },
            fetching: ActivityCount {
                observed: self.fetched,
                total: self.fetch_total,
            },
        })
    }

    fn parse(&mut self, line: &str, task: TaskId) -> Vec<UiEvent> {
        if let Kind::Copy { host } = &self.kind {
            if self.copy_total.is_none()
                && let Some(rest) = line.strip_prefix("copying ")
                && let Some((count, _)) = rest.split_once(" paths")
                && !count.is_empty()
                && count.bytes().all(|b| b.is_ascii_digit())
                && let Ok(count) = count.parse()
            {
                self.copy_total = Some(count);
            } else if line.starts_with("copying path '") {
                self.fetched = self.fetched.saturating_add(1);
            } else {
                return Vec::new();
            }
            let count = total(self.copy_total);
            let detail = format!("copy {}/{count}; {line}", self.fetched);
            return vec![
                UiEvent::PlainOutput {
                    stream: OutputStream::Stdout,
                    bytes: format!("[{host}] [{}/{count}] {detail}\n", self.fetched).into_bytes(),
                },
                UiEvent::TaskProgress {
                    task,
                    completed: self.fetched,
                    total: self.copy_total,
                    detail: format!("{host}: {detail}"),
                },
            ];
        }
        if line.starts_with("this derivation will be built:") {
            self.build_total = Some(1);
        } else if let Some(count) = count(line, " derivations will be built:") {
            self.build_total = Some(count);
        } else if line.starts_with("this path will be fetched ") {
            self.fetch_total = Some(1);
        } else if let Some(count) = count(line, " paths will be fetched ") {
            self.fetch_total = Some(count);
        } else if line.starts_with("these derivations will be built:") {
            self.build_total = Some(0);
            self.list = Some(true);
            return Vec::new();
        } else if line.starts_with("these paths will be fetched ") {
            self.fetch_total = Some(0);
            self.list = Some(false);
            return Vec::new();
        } else {
            if let Some(build) = self.list {
                let total = if build {
                    &mut self.build_total
                } else {
                    &mut self.fetch_total
                };
                if line.starts_with(char::is_whitespace)
                    && line.trim_start().starts_with("/nix/store/")
                {
                    *total = Some(total.unwrap_or(0).saturating_add(1));
                    return Vec::new();
                }
                *total = Some(total.unwrap_or(0).saturating_add(if build {
                    self.built
                } else {
                    self.fetched
                }));
                self.list = None;
            }
            if line.starts_with("building '") {
                self.built = self.built.saturating_add(1);
            } else if line.starts_with("copying path '") {
                self.fetched = self.fetched.saturating_add(1);
            } else {
                return Vec::new();
            }
        }
        self.snapshot(task).into_iter().collect()
    }
}

fn count(line: &str, suffix: &str) -> Option<u64> {
    let rest = line.strip_prefix("these ")?;
    let end = rest.find(suffix)?;
    rest[..end].parse().ok()
}
fn total(value: Option<u64>) -> String {
    value.map_or_else(|| "?".into(), |n| n.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn build_progress_is_incremental_bounded_and_attempt_local() {
        let task = TaskId(7);
        let mut decoder = Progress::default();
        assert!(matches!(
            decoder.snapshot(task),
            Some(UiEvent::BuildProgress {
                building: ActivityCount {
                    observed: 0,
                    total: None
                },
                fetching: ActivityCount {
                    observed: 0,
                    total: None
                },
                ..
            })
        ));
        assert!(decoder.feed(b"build", task).is_empty());
        let first = decoder.feed(b"ing '/nix/store/a.drv'...\n", task);
        assert!(matches!(
            first.as_slice(),
            [UiEvent::BuildProgress {
                building: ActivityCount {
                    observed: 1,
                    total: None
                },
                fetching: ActivityCount {
                    observed: 0,
                    total: None
                },
                ..
            }]
        ));
        let known = decoder.feed(b"these 2 derivations will be built:\nthis path will be fetched (1 MiB)\ncopying path '/nix/store/b'\n",task);
        assert!(matches!(
            known.last(),
            Some(UiEvent::BuildProgress {
                building: ActivityCount {
                    observed: 1,
                    total: Some(2)
                },
                fetching: ActivityCount {
                    observed: 1,
                    total: Some(1)
                },
                ..
            })
        ));
        assert!(decoder.feed(&vec![b'x'; LINE_LIMIT + 1], task).is_empty());
        assert!(decoder.feed(b"building 'false'\n", task).is_empty());
        assert!(decoder.line.is_empty());
        assert!(decoder.feed(b"unknown raw output\n\xff\n", task).is_empty());
        let mut next_attempt = Progress::default();
        assert!(matches!(
            next_attempt.snapshot(task),
            Some(UiEvent::BuildProgress {
                building: ActivityCount {
                    observed: 0,
                    total: None
                },
                fetching: ActivityCount {
                    observed: 0,
                    total: None
                },
                ..
            })
        ));
        assert!(matches!(
            next_attempt
                .feed(b"copying path '/nix/store/c'\n", task)
                .as_slice(),
            [UiEvent::BuildProgress {
                building: ActivityCount {
                    observed: 0,
                    total: None
                },
                fetching: ActivityCount {
                    observed: 1,
                    total: None
                },
                ..
            }]
        ));
    }
    #[test]
    fn build_old_lists_and_unterminated_activity_are_counted() {
        let mut decoder = Progress::default();
        decoder.feed(b"these derivations will be built:\n  /nix/store/a\n  /nix/store/b\nthese paths will be fetched (1 MiB):\n  /nix/store/c\n",TaskId(1));
        decoder.feed(
            b"building '/nix/store/a'\ncopying path '/nix/store/c'",
            TaskId(1),
        );
        assert!(matches!(
            decoder.flush(TaskId(1)).as_slice(),
            [UiEvent::BuildProgress {
                building: ActivityCount {
                    observed: 1,
                    total: Some(2)
                },
                fetching: ActivityCount {
                    observed: 1,
                    total: Some(1)
                },
                ..
            }]
        ));
    }
}

#[cfg(test)]
mod copy_tests {
    use super::*;
    #[test]
    fn copying_progress_is_chunked_bounded_and_keeps_unknown_totals() {
        let task = TaskId(8);
        let mut progress = Progress::new(Kind::Copy {
            host: "node".into(),
        });
        assert!(
            progress
                .feed(b"copying path '/store/early", task)
                .is_empty()
        );
        assert_eq!(
            progress.feed(b"'...\n", task),
            vec![
                UiEvent::PlainOutput {
                    stream: OutputStream::Stdout,
                    bytes: b"[node] [1/?] copy 1/?; copying path '/store/early'...\n".to_vec()
                },
                UiEvent::TaskProgress {
                    task,
                    completed: 1,
                    total: None,
                    detail: "node: copy 1/?; copying path '/store/early'...".into()
                },
            ]
        );
        assert_eq!(
            progress.feed(
                b"copying 3 paths...\r\ncopying path '/store/next'...\n",
                task
            ),
            vec![
                UiEvent::PlainOutput {
                    stream: OutputStream::Stdout,
                    bytes: b"[node] [1/3] copy 1/3; copying 3 paths...\n".to_vec()
                },
                UiEvent::TaskProgress {
                    task,
                    completed: 1,
                    total: Some(3),
                    detail: "node: copy 1/3; copying 3 paths...".into()
                },
                UiEvent::PlainOutput {
                    stream: OutputStream::Stdout,
                    bytes: b"[node] [2/3] copy 2/3; copying path '/store/next'...\n".to_vec()
                },
                UiEvent::TaskProgress {
                    task,
                    completed: 2,
                    total: Some(3),
                    detail: "node: copy 2/3; copying path '/store/next'...".into()
                },
            ]
        );
        assert!(progress.feed(&vec![b'x'; LINE_LIMIT + 1], task).is_empty());
        assert!(progress.feed(b"copying path 'false'\n", task).is_empty());
        assert!(progress.feed(b"unknown raw\n\xff\n", task).is_empty());
        assert!(
            Progress::new(Kind::Copy {
                host: "node".into()
            })
            .feed(b"copying +2 paths...\n", task)
            .is_empty()
        );
        assert!(
            progress
                .feed(b"copying path '/store/final'", task)
                .is_empty()
        );
        assert_eq!(
            progress.flush(task),
            vec![
                UiEvent::PlainOutput {
                    stream: OutputStream::Stdout,
                    bytes: b"[node] [3/3] copy 3/3; copying path '/store/final'\n".to_vec()
                },
                UiEvent::TaskProgress {
                    task,
                    completed: 3,
                    total: Some(3),
                    detail: "node: copy 3/3; copying path '/store/final'".into()
                },
            ]
        );
        assert!(
            progress.flush(task).is_empty(),
            "EOF emits each parsed line once"
        );
        let mut unknown = Progress::new(Kind::Copy {
            host: "node".into(),
        });
        assert!(unknown.feed(b"copying path '/store/only'", task).is_empty());
        assert_eq!(
            unknown.flush(task),
            vec![
                UiEvent::PlainOutput {
                    stream: OutputStream::Stdout,
                    bytes: b"[node] [1/?] copy 1/?; copying path '/store/only'\n".to_vec()
                },
                UiEvent::TaskProgress {
                    task,
                    completed: 1,
                    total: None,
                    detail: "node: copy 1/?; copying path '/store/only'".into()
                },
            ]
        );
        assert!(unknown.flush(task).is_empty());
    }
}
