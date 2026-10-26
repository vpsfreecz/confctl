//! Per-attempt Nix stderr interpretation. Captures and logs retain native bytes.

use crate::events::{NixActivityProgress, OutputStream, TaskId, UiEvent};
use serde_json::Value;
use std::collections::HashMap;

const LINE_LIMIT: usize = 8192;
const COPY_PATHS: u64 = 103;
const BUILDS: u64 = 104;
const PROGRESS: u64 = 105;
const SET_EXPECTED: u64 = 106;

#[derive(Clone, Default)]
pub(crate) enum Kind {
    #[default]
    Build,
    StructuredBuild,
    Copy {
        host: String,
    },
}

#[derive(Default)]
struct Activity {
    kind: u64,
    progress: Option<NixActivityProgress>,
    expected: HashMap<u64, u64>,
    stopped: bool,
}

#[derive(Default)]
pub(crate) struct Progress {
    kind: Kind,
    copy_total: Option<u64>,
    line: Vec<u8>,
    overflow: bool,
    fetched: u64,
    activities: HashMap<u64, Activity>,
    invalid_protocol: bool,
}

// The same interpretation serves live output and retained failure diagnostics.
// Unknown envelopes never become progress or hide a native failure.
enum Record {
    Text(Vec<u8>),
    Control(Value),
}
fn malformed() -> Vec<u8> {
    b"[unsupported or malformed Nix structured record; raw stderr retained]".to_vec()
}
fn record(line: &[u8]) -> Record {
    let Some(json) = line.strip_prefix(b"@nix ") else {
        return Record::Text(line.to_vec());
    };
    if let Ok(value) = serde_json::from_slice::<Value>(json) {
        let action = value.get("action").and_then(Value::as_str);
        let id = value.get("id").and_then(Value::as_u64);
        let kind = value.get("type").and_then(Value::as_u64);
        let fields = value.get("fields").and_then(Value::as_array);
        match action {
            Some("msg") => {
                if let Some(text) = value.get("msg").and_then(Value::as_str) {
                    return Record::Text(text.as_bytes().to_vec());
                }
            }
            Some("start")
                if id.is_some()
                    && kind.is_some()
                    && value.get("text").is_some_and(Value::is_string) =>
            {
                return Record::Control(value);
            }
            Some("stop") if id.is_some() => return Record::Control(value),
            Some("result") if id.is_some() => {
                if let (Some(kind), Some(fields)) = (kind, fields) {
                    match kind {
                        101 | 107 => {
                            if let Some(text) = fields.first().and_then(Value::as_str) {
                                return Record::Text(text.as_bytes().to_vec());
                            }
                        }
                        PROGRESS
                            if fields.len() == 4 && fields.iter().all(|v| v.as_u64().is_some()) =>
                        {
                            return Record::Control(value);
                        }
                        SET_EXPECTED
                            if fields.len() == 2 && fields.iter().all(|v| v.as_u64().is_some()) =>
                        {
                            return Record::Control(value);
                        }
                        // Known non-progress controls: linked files, verification and phase.
                        100 | 102..=104 => return Record::Control(value),
                        _ => {}
                    }
                }
            }
            _ => {}
        }
    }
    Record::Text(malformed())
}
fn readable_record(line: &[u8]) -> Vec<u8> {
    match record(line) {
        Record::Text(text) => text,
        Record::Control(value) => value
            .get("text")
            .and_then(Value::as_str)
            .unwrap_or("")
            .as_bytes()
            .to_vec(),
    }
}

/// Decode only Nix logging envelopes; ordinary stderr remains byte-for-byte.
pub(crate) fn readable_stderr(bytes: &[u8]) -> Vec<u8> {
    let mut output = Vec::new();
    for part in bytes.split_inclusive(|b| *b == b'\n') {
        if !part.starts_with(b"@nix ") {
            output.extend_from_slice(part);
            continue;
        }
        let newline = part.ends_with(b"\n");
        let line = part.strip_suffix(b"\n").unwrap_or(part);
        let text = if line.len() > LINE_LIMIT {
            b"[oversized Nix structured record; raw stderr retained]".to_vec()
        } else {
            readable_record(line)
        };
        if !text.is_empty() {
            output.extend(text);
            if newline {
                output.push(b'\n');
            }
        }
    }
    output
}

impl Progress {
    pub(crate) fn new(kind: Kind) -> Self {
        Self {
            kind,
            ..Self::default()
        }
    }
    pub(crate) fn structured(&self) -> bool {
        matches!(self.kind, Kind::StructuredBuild)
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
            return if self.structured() {
                self.invalid_protocol = true;
                let mut events = self.snapshot(task).into_iter().collect::<Vec<_>>();
                events.push(output(
                    task,
                    b"[oversized Nix stderr line; raw stderr retained]".to_vec(),
                ));
                events
            } else {
                Vec::new()
            };
        }
        if line.is_empty() {
            return Vec::new();
        }
        if self.structured() {
            return self.structured_line(&line, task);
        }
        let Ok(line) = std::str::from_utf8(&line) else {
            return Vec::new();
        };
        self.parse_copy(line.trim_end_matches('\r'), task)
    }
    pub(crate) fn snapshot(&self, task: TaskId) -> Option<UiEvent> {
        matches!(self.kind, Kind::Build | Kind::StructuredBuild).then(|| UiEvent::BuildProgress {
            task,
            building: self.category(BUILDS),
            fetching: self.category(COPY_PATHS),
        })
    }
    fn category(&self, kind: u64) -> Option<NixActivityProgress> {
        if self.invalid_protocol {
            return None;
        }
        let mut sum = NixActivityProgress::default();
        let mut known = false;
        let mut typed_expected = 0u64;
        for activity in self.activities.values() {
            if !activity.stopped
                && let Some(expected) = activity.expected.get(&kind)
            {
                typed_expected = typed_expected.checked_add(*expected)?;
                known = true;
            }
            if activity.kind == kind
                && let Some(progress) = activity.progress
            {
                sum.done = sum.done.checked_add(progress.done)?;
                sum.expected = sum.expected.checked_add(progress.expected)?;
                sum.running = sum.running.checked_add(progress.running)?;
                sum.failed = sum.failed.checked_add(progress.failed)?;
                known = true;
            }
        }
        sum.expected = sum.expected.max(typed_expected);
        known.then_some(sum)
    }
    fn structured_line(&mut self, line: &[u8], task: TaskId) -> Vec<UiEvent> {
        let mut events = Vec::new();
        match record(line) {
            Record::Text(text) => {
                if line.starts_with(b"@nix ") && text == malformed() {
                    self.invalid_protocol = true;
                    events.extend(self.snapshot(task));
                }
                if !text.is_empty() {
                    events.push(output(task, text));
                }
            }
            Record::Control(value) => {
                let id = value.get("id").and_then(Value::as_u64);
                match value.get("action").and_then(Value::as_str) {
                    Some("start") => {
                        if let (Some(id), Some(kind)) =
                            (id, value.get("type").and_then(Value::as_u64))
                        {
                            let activity = self.activities.entry(id).or_insert_with(|| Activity {
                                kind,
                                ..Activity::default()
                            });
                            if activity.stopped {
                                return events;
                            }
                        }
                        if let Some(text) = value
                            .get("text")
                            .and_then(Value::as_str)
                            .filter(|v| !v.is_empty())
                        {
                            events.push(output(task, text.as_bytes().to_vec()));
                        }
                    }
                    Some("result") => {
                        if let (Some(activity), Some(fields)) = (
                            id.and_then(|id| self.activities.get_mut(&id))
                                .filter(|activity| !activity.stopped),
                            value.get("fields").and_then(Value::as_array),
                        ) {
                            match value.get("type").and_then(Value::as_u64) {
                                Some(PROGRESS) => {
                                    // Replaces the previous snapshot. Malformed values invalidate it.
                                    activity.progress = (|| {
                                        Some(NixActivityProgress {
                                            done: fields.first()?.as_u64()?,
                                            expected: fields.get(1)?.as_u64()?,
                                            running: fields.get(2)?.as_u64()?,
                                            failed: fields.get(3)?.as_u64()?,
                                        })
                                    })();
                                }
                                Some(SET_EXPECTED) => {
                                    if let (Some(kind), Some(expected)) = (
                                        fields.first().and_then(Value::as_u64),
                                        fields.get(1).and_then(Value::as_u64),
                                    ) {
                                        activity.expected.insert(kind, expected);
                                    }
                                }
                                _ => {}
                            }
                            events.extend(self.snapshot(task));
                        }
                    }
                    // A stop is not a successful completion. Retain last done/failed,
                    // but stopped work is no longer running or predicting extra work.
                    Some("stop") => {
                        if let Some(activity) = id
                            .and_then(|id| self.activities.get_mut(&id))
                            .filter(|activity| !activity.stopped)
                        {
                            if let Some(progress) = activity.progress.as_mut() {
                                progress.expected = progress.done;
                                progress.running = 0;
                            }
                            activity.expected.clear();
                            activity.stopped = true;
                            events.extend(self.snapshot(task));
                        }
                    }
                    _ => {}
                }
            }
        }
        events
    }
    fn parse_copy(&mut self, line: &str, task: TaskId) -> Vec<UiEvent> {
        let Kind::Copy { host } = &self.kind else {
            return Vec::new();
        };
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
        vec![
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
        ]
    }
}
fn output(task: TaskId, mut bytes: Vec<u8>) -> UiEvent {
    if !bytes.ends_with(b"\n") {
        bytes.push(b'\n');
    }
    UiEvent::ActivityOutput {
        task,
        stream: OutputStream::Stderr,
        bytes,
    }
}
fn total(value: Option<u64>) -> String {
    value.map_or_else(|| "?".into(), |n| n.to_string())
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

#[cfg(test)]
mod structured_tests {
    use super::*;
    fn feed(progress: &mut Progress, value: Value) -> Vec<UiEvent> {
        let mut bytes = b"@nix ".to_vec();
        bytes.extend(serde_json::to_vec(&value).unwrap());
        bytes.push(b'\n');
        progress.feed(&bytes, TaskId(1))
    }
    fn start(progress: &mut Progress, id: u64, kind: u64) {
        feed(
            progress,
            serde_json::json!({"action":"start","id":id,"type":kind,"text":""}),
        );
    }
    fn update(progress: &mut Progress, id: u64, counts: [u64; 4]) {
        feed(
            progress,
            serde_json::json!({"action":"result","id":id,"type":PROGRESS,"fields":counts}),
        );
    }
    fn expected(progress: &mut Progress, owner: u64, kind: u64, count: u64) {
        feed(
            progress,
            serde_json::json!({"action":"result","id":owner,"type":SET_EXPECTED,"fields":[kind,count]}),
        );
    }
    fn stop(progress: &mut Progress, id: u64) -> Vec<UiEvent> {
        feed(progress, serde_json::json!({"action":"stop","id":id}))
    }
    fn counts(progress: &Progress, kind: u64) -> [u64; 4] {
        let p = progress.category(kind).expect("known category");
        [p.done, p.expected, p.running, p.failed]
    }
    #[test]
    fn structured_snapshots_replace_and_stop_never_infers_completion() {
        let mut progress = Progress::new(Kind::StructuredBuild);
        assert_eq!(
            progress.snapshot(TaskId(1)),
            Some(UiEvent::BuildProgress {
                task: TaskId(1),
                building: None,
                fetching: None
            })
        );
        start(&mut progress, 1, BUILDS);
        start(&mut progress, 2, COPY_PATHS);
        start(&mut progress, 3, 105);
        let value = serde_json::json!({"action":"result","id":1,"type":105,"fields":[2,5,2,1]});
        feed(&mut progress, value.clone());
        feed(&mut progress, value);
        // A child derivation's activity does not increment aggregate completion.
        assert_eq!(
            progress.category(BUILDS),
            Some(NixActivityProgress {
                done: 2,
                expected: 5,
                running: 2,
                failed: 1
            })
        );
        feed(
            &mut progress,
            serde_json::json!({"action":"result","id":3,"type":106,"fields":[104,9]}),
        );
        assert_eq!(progress.category(BUILDS).unwrap().expected, 9);
        feed(
            &mut progress,
            serde_json::json!({"action":"result","id":2,"type":105,"fields":[1,4,1,0]}),
        );
        feed(&mut progress, serde_json::json!({"action":"stop","id":1}));
        assert_eq!(
            progress.category(BUILDS),
            Some(NixActivityProgress {
                done: 2,
                expected: 9,
                running: 0,
                failed: 1
            })
        );
        assert_eq!(progress.category(COPY_PATHS).unwrap().done, 1);
        stop(&mut progress, 3);
        assert_eq!(counts(&progress, BUILDS), [2, 2, 0, 1]);
        assert_eq!(Progress::new(Kind::StructuredBuild).category(BUILDS), None);
    }
    #[test]
    fn stopped_estimates_retire_only_when_live_expectation_owners_are_gone() {
        let mut progress = Progress::new(Kind::StructuredBuild);
        start(&mut progress, 1, BUILDS);
        start(&mut progress, 2, COPY_PATHS);
        start(&mut progress, 3, 0);
        update(&mut progress, 1, [11, 12, 1, 0]);
        update(&mut progress, 2, [1, 1, 0, 0]);
        expected(&mut progress, 3, BUILDS, 12);
        stop(&mut progress, 1);
        stop(&mut progress, 2);
        assert_eq!(counts(&progress, BUILDS), [11, 12, 0, 0]);
        assert_eq!(counts(&progress, COPY_PATHS), [1, 1, 0, 0]);
        stop(&mut progress, 3);
        assert_eq!(counts(&progress, BUILDS), [11, 11, 0, 0]);
        let builds = progress.category(BUILDS).unwrap();
        let fetches = progress.category(COPY_PATHS).unwrap();
        assert_eq!(builds.done + fetches.done, 12);
        assert_eq!(builds.expected + fetches.expected, 12);
        assert_eq!(builds.done, 11, "retirement never invents a build");

        let snapshot = progress.snapshot(TaskId(1));
        assert!(stop(&mut progress, 1).is_empty());
        assert!(stop(&mut progress, 99).is_empty());
        assert!(
            feed(
                &mut progress,
                serde_json::json!({"action":"result","id":1,"type":PROGRESS,"fields":[12,99,4,9]}),
            )
            .is_empty()
        );
        assert!(
            feed(
                &mut progress,
                serde_json::json!({"action":"result","id":3,"type":SET_EXPECTED,"fields":[BUILDS,99]}),
            )
            .is_empty()
        );
        assert!(
            feed(
                &mut progress,
                serde_json::json!({"action":"start","id":1,"type":BUILDS,"text":"late restart"}),
            )
            .is_empty()
        );
        assert_eq!(progress.snapshot(TaskId(1)), snapshot);
    }
    #[test]
    fn nested_owners_replace_their_own_estimates_without_retiring_children() {
        let mut progress = Progress::new(Kind::StructuredBuild);
        start(&mut progress, 1, 0);
        for (id, parent, kind) in [(2, 1, 0), (3, 2, BUILDS), (4, 2, BUILDS), (5, 3, 105)] {
            feed(
                &mut progress,
                serde_json::json!({"action":"start","id":id,"parent":parent,"type":kind,"text":""}),
            );
        }
        expected(&mut progress, 1, BUILDS, 7);
        expected(&mut progress, 2, BUILDS, 5);
        update(&mut progress, 3, [2, 5, 1, 1]);
        update(&mut progress, 4, [1, 3, 1, 0]);
        update(&mut progress, 5, [100, 100, 0, 0]);
        assert_eq!(counts(&progress, BUILDS), [3, 12, 2, 1]);
        expected(&mut progress, 1, BUILDS, 4);
        update(&mut progress, 3, [4, 6, 1, 2]);
        assert_eq!(counts(&progress, BUILDS), [5, 9, 2, 2]);
        stop(&mut progress, 2);
        assert_eq!(counts(&progress, BUILDS), [5, 9, 2, 2]);
        stop(&mut progress, 3);
        assert_eq!(counts(&progress, BUILDS), [5, 7, 1, 2]);
        stop(&mut progress, 1);
        assert_eq!(counts(&progress, BUILDS), [5, 7, 1, 2]);
        stop(&mut progress, 4);
        assert_eq!(counts(&progress, BUILDS), [5, 5, 0, 2]);
    }
    #[test]
    fn chunked_stop_at_eof_preserves_unknown_and_last_observed_counts() {
        let mut progress = Progress::new(Kind::StructuredBuild);
        assert!(stop(&mut progress, 99).is_empty());
        start(&mut progress, 1, BUILDS);
        stop(&mut progress, 1);
        assert_eq!(progress.category(BUILDS), None);

        let raw = b"@nix {\"action\":\"start\",\"id\":2,\"type\":104,\"text\":\"\"}\n@nix {\"action\":\"result\",\"id\":2,\"type\":105,\"fields\":[11,12,1,0]}";
        for chunk in raw.chunks(7) {
            progress.feed(chunk, TaskId(1));
        }
        assert_eq!(progress.category(BUILDS), None);
        progress.flush(TaskId(1));
        assert_eq!(counts(&progress, BUILDS), [11, 12, 1, 0]);
        assert!(progress.flush(TaskId(1)).is_empty());
        assert_eq!(counts(&progress, BUILDS), [11, 12, 1, 0]);
        for chunk in b"@nix {\"action\":\"stop\",\"id\":2}".chunks(3) {
            assert!(progress.feed(chunk, TaskId(1)).is_empty());
        }
        assert_eq!(counts(&progress, BUILDS), [11, 12, 1, 0]);
        assert_eq!(
            progress.flush(TaskId(1)),
            vec![UiEvent::BuildProgress {
                task: TaskId(1),
                building: Some(NixActivityProgress {
                    done: 11,
                    expected: 11,
                    running: 0,
                    failed: 0,
                }),
                fetching: None,
            }]
        );
        assert!(progress.flush(TaskId(1)).is_empty());
        assert_eq!(Progress::new(Kind::StructuredBuild).category(BUILDS), None);
    }
    #[test]
    fn structured_chunk_eof_and_readable_errors_keep_raw_text_without_json() {
        let raw = b"ordinary\n@nix {\"action\":\"msg\",\"level\":0,\"msg\":\"error: unsupported --no-update-lock-file\"}\n@nix {\"action\":\"result\",\"id\":2,\"type\":101,\"fields\":[\"compiler line\"]}\n@nix {\"action\":\"msg\",\"msg\":\"final trace\"}";
        assert_eq!(
            readable_stderr(raw),
            b"ordinary\nerror: unsupported --no-update-lock-file\ncompiler line\nfinal trace"
        );
        let mut decoder = Progress::new(Kind::StructuredBuild);
        let mut events = Vec::new();
        for chunk in raw.chunks(7) {
            events.extend(decoder.feed(chunk, TaskId(1)));
        }
        events.extend(decoder.flush(TaskId(1)));
        assert!(decoder.flush(TaskId(1)).is_empty());
        let live = events
            .into_iter()
            .filter_map(|event| match event {
                UiEvent::ActivityOutput { bytes, .. } => Some(bytes),
                _ => None,
            })
            .flatten()
            .collect::<Vec<_>>();
        assert_eq!(
            live,
            b"ordinary\nerror: unsupported --no-update-lock-file\ncompiler line\nfinal trace\n"
        );
        assert_eq!(
            readable_stderr(b"raw\xff\nno newline"),
            b"raw\xff\nno newline"
        );
    }
    #[test]
    fn structured_unknown_malformed_overflow_and_human_starts_never_invent_done() {
        let mut decoder = Progress::new(Kind::StructuredBuild);
        decoder.feed(b"building '/store/x'\ncopying path '/store/y'\n", TaskId(1));
        assert_eq!(decoder.category(BUILDS), None);
        let diagnostics = readable_stderr(b"@nix invalid\n@nix {\"action\":\"new-protocol\"}\n");
        assert!(String::from_utf8_lossy(&diagnostics).contains("malformed"));
        start(&mut decoder, 1, BUILDS);
        start(&mut decoder, 2, BUILDS);
        feed(
            &mut decoder,
            serde_json::json!({"action":"result","id":1,"type":105,"fields":[u64::MAX,u64::MAX,0,0]}),
        );
        feed(
            &mut decoder,
            serde_json::json!({"action":"result","id":2,"type":105,"fields":[1,1,0,0]}),
        );
        assert_eq!(
            decoder.category(BUILDS),
            None,
            "checked aggregate overflow is unknown"
        );
        decoder.feed(&vec![b'x'; LINE_LIMIT + 1], TaskId(1));
        assert!(matches!(
            decoder.flush(TaskId(1)).as_slice(),
            [
                UiEvent::BuildProgress { .. },
                UiEvent::ActivityOutput { .. }
            ]
        ));
    }
}
