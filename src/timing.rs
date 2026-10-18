//! Invocation timing uses unions of monotonic intervals, not worker sums.
//! Admission follows the parsed action; presentation cannot change command policy.

use crate::domain::{Action, ExitDisposition, ExitState};
use chrono::{DateTime, FixedOffset, Local};
use std::{
    collections::BTreeMap,
    io::{self, Write},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum Phase {
    Evaluation,
    Build,
    Copy,
    Deployment,
    StatusChecks,
    RebootWait,
    HealthChecks,
    SshCommands,
    ConnectionTests,
    GarbageCollection,
}

impl Phase {
    pub const ALL: [Self; 10] = [
        Self::Evaluation,
        Self::Build,
        Self::Copy,
        Self::Deployment,
        Self::StatusChecks,
        Self::RebootWait,
        Self::HealthChecks,
        Self::SshCommands,
        Self::ConnectionTests,
        Self::GarbageCollection,
    ];

    pub const fn label(self) -> &'static str {
        match self {
            Self::Evaluation => "Evaluation",
            Self::Build => "Build",
            Self::Copy => "Copy",
            Self::Deployment => "Deployment",
            Self::StatusChecks => "Status checks",
            Self::RebootWait => "Reboot wait",
            Self::HealthChecks => "Health checks",
            Self::SshCommands => "SSH commands",
            Self::ConnectionTests => "Connection tests",
            Self::GarbageCollection => "Garbage collection",
        }
    }
}

pub trait Clock: Send + Sync {
    fn monotonic(&self) -> Duration;
    fn wall(&self) -> DateTime<FixedOffset>;
}

pub struct SystemClock {
    origin: Instant,
}

impl Default for SystemClock {
    fn default() -> Self {
        Self {
            origin: Instant::now(),
        }
    }
}

impl Clock for SystemClock {
    fn monotonic(&self) -> Duration {
        self.origin.elapsed()
    }
    fn wall(&self) -> DateTime<FixedOffset> {
        Local::now().fixed_offset()
    }
}

type Interval = (Duration, Duration);

#[derive(Default)]
struct Ledger {
    admitted: bool,
    finished: bool,
    next_id: u64,
    active: BTreeMap<u64, (Option<Phase>, Duration)>,
    phases: BTreeMap<Phase, Vec<Interval>>,
    prompts: Vec<Interval>,
}

struct Inner {
    clock: Arc<dyn Clock>,
    origin: Duration,
    started: DateTime<FixedOffset>,
    ledger: Mutex<Ledger>,
}

#[derive(Clone)]
pub struct Timing {
    inner: Arc<Inner>,
}

pub struct PhaseGuard {
    inner: Arc<Inner>,
    id: Option<u64>,
}
pub type PromptGuard = PhaseGuard;

impl Timing {
    pub fn start() -> Self {
        Self::with_clock(Arc::new(SystemClock::default()))
    }

    pub fn with_clock(clock: Arc<dyn Clock>) -> Self {
        let origin = clock.monotonic();
        let started = clock.wall();
        Self {
            inner: Arc::new(Inner {
                clock,
                origin,
                started,
                ledger: Mutex::new(Ledger::default()),
            }),
        }
    }

    /// Call after successful parsing. Action owns the command admission policy.
    pub fn admit(&self, action: &Action) {
        let mut ledger = self
            .inner
            .ledger
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        if !ledger.finished {
            ledger.admitted = action.timing_admitted();
        }
    }

    pub fn elapsed(&self) -> Duration {
        self.inner
            .clock
            .monotonic()
            .saturating_sub(self.inner.origin)
    }
    pub fn started(&self) -> DateTime<FixedOffset> {
        self.inner.started
    }
    pub fn phase(&self, phase: Phase) -> PhaseGuard {
        self.guard(Some(phase))
    }
    pub fn prompt(&self) -> PromptGuard {
        self.guard(None)
    }

    fn guard(&self, phase: Option<Phase>) -> PhaseGuard {
        let now = self.elapsed();
        let mut ledger = self
            .inner
            .ledger
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        let id = if ledger.finished || !ledger.admitted {
            None
        } else {
            let id = ledger.next_id;
            ledger.next_id += 1;
            ledger.active.insert(id, (phase, now));
            Some(id)
        };
        PhaseGuard {
            inner: self.inner.clone(),
            id,
        }
    }

    pub fn finish(self, exit: ExitDisposition) -> Option<TimingReport> {
        let mut ledger = self
            .inner
            .ledger
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        if ledger.finished {
            return None;
        }
        // Serialize the cutoff with guard closure: no completed interval may
        // be recorded after the sampled total but before this ledger freezes.
        let total = self.elapsed();
        let ended = self.inner.clock.wall();
        ledger.finished = true;
        for (_, (phase, start)) in std::mem::take(&mut ledger.active) {
            record(&mut ledger, phase, (start, total));
        }
        if !ledger.admitted {
            return None;
        }
        let phases: BTreeMap<_, _> = ledger
            .phases
            .iter()
            .map(|(phase, intervals)| (*phase, union(intervals)))
            .collect();
        let work_intervals: Vec<_> = ledger.phases.values().flatten().copied().collect();
        let work = union(&work_intervals);
        let prompt = union(&ledger.prompts);
        let mut occupied = work_intervals;
        occupied.extend_from_slice(&ledger.prompts);
        let occupied = union(&occupied);
        let overlap = work.saturating_add(prompt).saturating_sub(occupied);
        let note = phases.values().copied().sum::<Duration>() > work;
        Some(TimingReport {
            exit,
            started: self.inner.started,
            ended,
            total,
            phases,
            prompt,
            overlap,
            other: total.saturating_sub(occupied),
            overlapping_phases: note,
        })
    }
}

impl PhaseGuard {
    pub fn end(mut self) {
        self.close();
    }

    fn close(&mut self) {
        let Some(id) = self.id.take() else {
            return;
        };
        let now = self
            .inner
            .clock
            .monotonic()
            .saturating_sub(self.inner.origin);
        let mut ledger = self
            .inner
            .ledger
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        if let Some((phase, start)) = ledger.active.remove(&id) {
            record(&mut ledger, phase, (start, now));
        }
    }
}

impl Drop for PhaseGuard {
    fn drop(&mut self) {
        self.close();
    }
}

fn record(ledger: &mut Ledger, phase: Option<Phase>, interval: Interval) {
    if let Some(phase) = phase {
        ledger.phases.entry(phase).or_default().push(interval);
    } else {
        ledger.prompts.push(interval);
    }
}

fn union(intervals: &[Interval]) -> Duration {
    let mut sorted = intervals.to_vec();
    sorted.sort_unstable();
    let mut total = Duration::ZERO;
    let mut current: Option<Interval> = None;
    for (start, end) in sorted {
        if end < start {
            continue;
        }
        match current {
            Some((left, right)) if start <= right => current = Some((left, right.max(end))),
            Some((left, right)) => {
                total += right - left;
                current = Some((start, end));
            }
            None => current = Some((start, end)),
        }
    }
    if let Some((start, end)) = current {
        total += end - start;
    }
    total
}

pub struct TimingReport {
    pub exit: ExitDisposition,
    pub started: DateTime<FixedOffset>,
    pub ended: DateTime<FixedOffset>,
    pub total: Duration,
    pub phases: BTreeMap<Phase, Duration>,
    pub prompt: Duration,
    pub overlap: Duration,
    pub other: Duration,
    pub overlapping_phases: bool,
}

impl TimingReport {
    /// Call once, on original stderr, only after operation/log/terminal joins.
    /// The caller retains the error; no recursive report or retry is attempted.
    pub fn write(self, writer: &mut impl Write) -> Result<(), ReportWriteFailure> {
        let result = self.write_inner(writer);
        result.map_err(|source| ReportWriteFailure {
            code: if self.exit.code == 0 {
                1
            } else {
                self.exit.code
            },
            source,
        })
    }

    fn write_inner(&self, writer: &mut impl Write) -> io::Result<()> {
        let state = match self.exit.state {
            ExitState::Completed => "completed",
            ExitState::Failed => "failed",
            ExitState::Canceled => "canceled",
        };
        writeln!(writer, "\nTiming: {state} (exit {})", self.exit.code)?;
        writeln!(
            writer,
            "  Started: {}",
            self.started
                .to_rfc3339_opts(chrono::SecondsFormat::Secs, false)
        )?;
        writeln!(
            writer,
            "  Ended: {}",
            self.ended
                .to_rfc3339_opts(chrono::SecondsFormat::Secs, false)
        )?;
        for phase in Phase::ALL {
            if let Some(duration) = self.phases.get(&phase) {
                writeln!(
                    writer,
                    "  {}: {}",
                    phase.label(),
                    format_duration(*duration)
                )?;
            }
        }
        writeln!(writer, "  Prompt wait: {}", format_duration(self.prompt))?;
        if !self.overlap.is_zero() {
            writeln!(
                writer,
                "  Prompt/work overlap: {}",
                format_duration(self.overlap)
            )?;
        }
        writeln!(writer, "  Other: {}", format_duration(self.other))?;
        writeln!(writer, "  Total: {}", format_duration(self.total))?;
        if self.overlapping_phases {
            writeln!(writer, "  Note: phase times overlap; total is wall time.")?;
        }
        writer.flush()
    }
}

#[derive(Debug)]
pub struct ReportWriteFailure {
    pub code: i32,
    pub source: io::Error,
}

pub fn format_duration(duration: Duration) -> String {
    let seconds = duration.as_secs();
    let fraction = duration.subsec_millis();
    if seconds >= 3600 {
        format!(
            "{}h {}m {}.{fraction:03}s",
            seconds / 3600,
            seconds / 60 % 60,
            seconds % 60
        )
    } else if seconds >= 60 {
        format!("{}m {}.{fraction:03}s", seconds / 60, seconds % 60)
    } else {
        format!("{seconds}.{fraction:03}s")
    }
}

pub fn format_elapsed(duration: Duration) -> String {
    let seconds = duration.as_secs();
    if seconds >= 3600 {
        format!(
            "{}h {}m {}s",
            seconds / 3600,
            seconds / 60 % 60,
            seconds % 60
        )
    } else if seconds >= 60 {
        format!("{}m {}s", seconds / 60, seconds % 60)
    } else {
        format!("{seconds}s")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicU64, Ordering};

    struct FakeClock {
        millis: AtomicU64,
    }
    impl FakeClock {
        fn set(&self, millis: u64) {
            self.millis.store(millis, Ordering::SeqCst);
        }
    }
    impl Clock for FakeClock {
        fn monotonic(&self) -> Duration {
            Duration::from_millis(self.millis.load(Ordering::SeqCst))
        }
        fn wall(&self) -> DateTime<FixedOffset> {
            "2026-10-18T12:34:56+02:00"
                .parse()
                .expect("fixed wall time")
        }
    }

    #[test]
    fn unions_parallel_nested_work_and_prompt_overlap_then_freezes() {
        let clock = Arc::new(FakeClock {
            millis: AtomicU64::new(0),
        });
        let timing = Timing::with_clock(clock.clone());
        timing.admit(&Action::CollectGarbage {
            targets: Default::default(),
            max_concurrent: 1,
        });
        let build = timing.phase(Phase::Build);
        clock.set(1000);
        let second_build = timing.phase(Phase::Build);
        let eval = timing.phase(Phase::Evaluation);
        clock.set(2000);
        let prompt = timing.prompt();
        build.end();
        clock.set(3000);
        second_build.end();
        eval.end();
        clock.set(4000);
        prompt.end();
        clock.set(5000);
        let late = timing.clone();
        let report = timing
            .finish(ExitDisposition::completed())
            .expect("admitted");
        assert_eq!(report.phases[&Phase::Build], Duration::from_secs(3));
        assert_eq!(report.phases[&Phase::Evaluation], Duration::from_secs(2));
        assert_eq!(report.prompt, Duration::from_secs(2));
        assert_eq!(report.overlap, Duration::from_secs(1));
        assert_eq!(report.other, Duration::from_secs(1));
        assert!(report.overlapping_phases);
        clock.set(9000);
        drop(late.phase(Phase::Copy));
        assert!(late.finish(ExitDisposition::failed(1)).is_none());
        let mut bytes = Vec::new();
        report.write(&mut bytes).expect("write");
        assert_eq!(
            String::from_utf8(bytes).expect("utf8"),
            "\nTiming: completed (exit 0)\n  Started: 2026-10-18T12:34:56+02:00\n  Ended: 2026-10-18T12:34:56+02:00\n  Evaluation: 2.000s\n  Build: 3.000s\n  Prompt wait: 2.000s\n  Prompt/work overlap: 1.000s\n  Other: 1.000s\n  Total: 5.000s\n  Note: phase times overlap; total is wall time.\n"
        );
    }

    #[test]
    fn denied_admission_and_failed_footer_do_not_invent_success() {
        assert!(
            Timing::start()
                .finish(ExitDisposition::completed())
                .is_none()
        );
        let excluded = Timing::start();
        excluded.admit(&Action::Init);
        assert!(excluded.finish(ExitDisposition::completed()).is_none());
        struct Broken;
        impl Write for Broken {
            fn write(&mut self, _: &[u8]) -> io::Result<usize> {
                Err(io::ErrorKind::BrokenPipe.into())
            }
            fn flush(&mut self) -> io::Result<()> {
                Ok(())
            }
        }
        for exit in [
            ExitDisposition::completed(),
            ExitDisposition::canceled(143),
            ExitDisposition::failed(64),
        ] {
            let timing = Timing::start();
            timing.admit(&Action::CollectGarbage {
                targets: Default::default(),
                max_concurrent: 1,
            });
            let failure = timing
                .finish(exit)
                .expect("report")
                .write(&mut Broken)
                .expect_err("broken pipe");
            assert_eq!(failure.code, if exit.code == 0 { 1 } else { exit.code });
        }
    }

    #[test]
    fn freeze_serializes_with_concurrent_guard_close_before_sampling_cutoff() {
        use std::sync::mpsc;

        struct FreezeClock {
            millis: AtomicU64,
            entered: mpsc::SyncSender<()>,
            release: Mutex<mpsc::Receiver<()>>,
            closing: mpsc::SyncSender<()>,
        }
        impl Clock for FreezeClock {
            fn monotonic(&self) -> Duration {
                let sampled = self.millis.load(Ordering::SeqCst);
                match std::thread::current().name() {
                    Some("timing-freeze") => {
                        self.entered.send(()).expect("freeze sampled");
                        self.release
                            .lock()
                            .expect("release receiver")
                            .recv()
                            .expect("release freeze");
                    }
                    Some("timing-close") => self.closing.send(()).expect("close sampled"),
                    _ => {}
                }
                Duration::from_millis(sampled)
            }
            fn wall(&self) -> DateTime<FixedOffset> {
                "2026-10-18T12:34:56+02:00".parse().expect("wall time")
            }
        }
        let (entered, at_cutoff) = mpsc::sync_channel(1);
        let (release, wait) = mpsc::channel();
        let (closing, close_sampled) = mpsc::sync_channel(1);
        let clock = Arc::new(FreezeClock {
            millis: AtomicU64::new(0),
            entered,
            release: Mutex::new(wait),
            closing,
        });
        let timing = Timing::with_clock(clock.clone());
        timing.admit(&Action::CollectGarbage {
            targets: Default::default(),
            max_concurrent: 1,
        });
        let guard = timing.phase(Phase::Build);
        clock.millis.store(1000, Ordering::SeqCst);
        let freezing = timing.clone();
        let finish = std::thread::Builder::new()
            .name("timing-freeze".into())
            .spawn(move || {
                freezing
                    .finish(ExitDisposition::completed())
                    .expect("report")
            })
            .expect("finish thread");
        at_cutoff
            .recv_timeout(Duration::from_secs(5))
            .expect("freeze entered");
        // This ordering assertion makes the former race reproducible without
        // relying on a sleep or requiring one thread to win a lock race.
        let cutoff_holds_ledger = timing.inner.ledger.try_lock().is_err();
        clock.millis.store(2000, Ordering::SeqCst);
        let close = std::thread::Builder::new()
            .name("timing-close".into())
            .spawn(move || drop(guard))
            .expect("close thread");
        close_sampled
            .recv_timeout(Duration::from_secs(5))
            .expect("concurrent close sampled later time");
        release.send(()).expect("unblock freeze");
        let report = finish.join().expect("finish joined");
        close.join().expect("guard close joined");
        assert!(
            cutoff_holds_ledger,
            "guard closure must serialize with cutoff sampling"
        );
        assert_eq!(report.total, Duration::from_secs(1));
        assert_eq!(report.phases[&Phase::Build], Duration::from_secs(1));
        assert!(report.other.is_zero());
    }

    #[test]
    fn duration_format_truncates_and_does_not_round() {
        assert_eq!(format_duration(Duration::from_nanos(12_999_999)), "0.012s");
        assert_eq!(
            format_duration(Duration::from_millis(3_601_999)),
            "1h 0m 1.999s"
        );
        assert_eq!(format_elapsed(Duration::from_millis(61_999)), "1m 1s");
    }
}
