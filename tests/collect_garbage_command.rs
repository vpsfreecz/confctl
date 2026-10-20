#[path = "support/cleanup_fixture.rs"]
pub mod fixture;
use fixture::Fixture;
use std::{
    fs,
    io::{self, Read, Write},
    os::{
        fd::OwnedFd,
        unix::{ffi::OsStrExt, process::ExitStatusExt},
    },
    path::Path,
    process::{Child, Command, ExitStatus, Stdio},
    time::{Duration, Instant},
};
fn binary() -> &'static Path {
    Path::new(env!("CARGO_BIN_EXE_confctl-rust"))
}
#[test]
fn gc_uses_runnable_unmanaged_targets_and_native_failures_continue_then_aggregate() {
    let f = Fixture::new();
    fs::write(f.root.join("a.fail"), b"").unwrap();
    let out = f.run(
        binary(),
        &[
            "collect-garbage",
            "--yes",
            "--max-concurrent-gc=1",
            "--tag=chosen",
        ],
        &[],
    );
    assert!(!out.status.success());
    let calls = f.calls();
    assert_eq!(calls.matches("nix-collect-garbage").count(), 2);
    assert!(!calls.contains(" -d"));
    let text = String::from_utf8_lossy(&out.stdout);
    assert!(
        text.contains("alpha> deleting")
            && text.contains("beta> deleting")
            && text.contains("alpha: error")
            && text.contains("beta: 2 store paths deleted")
    );
    assert!(String::from_utf8_lossy(&out.stderr).contains("alpha"));
    assert!(
        fs::read_to_string(f.root.join("concurrency"))
            .unwrap()
            .lines()
            .all(|v| v == "1")
    );
    let f = Fixture::new();
    let out = f.run(binary(), &["collect-garbage", "alpha"], b"n\n");
    assert!(!out.status.success());
    assert!(!f.root.join(".confctl/mutation.lock").exists());
    f.no_deletions();
    for limit in ["0", "-1"] {
        let f = Fixture::new();
        assert!(
            !f.run(
                binary(),
                &[
                    "collect-garbage",
                    "--yes",
                    "--max-concurrent-gc",
                    limit,
                    "alpha"
                ],
                &[]
            )
            .status
            .success()
        );
        f.no_deletions();
    }
    let f = Fixture::new();
    assert!(
        !f.run(binary(), &["collect-garbage", "--yes", "--tag=guest"], &[])
            .status
            .success()
    );
    f.no_deletions();
}
#[test]
fn both_streams_are_labelled_before_two_gc_children_exit_and_cancel_joins_under_guard() {
    for cancel in [false, true] {
        let f = Fixture::new();
        fs::write(f.root.join("gate"), b"").unwrap();
        let stdout = f.root.join("stdout");
        let stderr = f.root.join("stderr");
        let mut child = f
            .command(
                binary(),
                &[
                    "collect-garbage",
                    "--yes",
                    "--max-concurrent-gc=2",
                    "--tag=chosen",
                ],
            )
            .stdout(fs::File::create(&stdout).unwrap())
            .stderr(fs::File::create(&stderr).unwrap())
            .spawn()
            .unwrap();
        drop(child.stdin.take());
        let deadline = Instant::now() + Duration::from_secs(8);
        loop {
            let out = fs::read(&stdout).unwrap();
            let err = fs::read(&stderr).unwrap();
            if [b"alpha> deleting".as_slice(), b"beta> deleting"]
                .iter()
                .all(|s| {
                    out.windows(s.len()).any(|w| w == *s) && err.windows(s.len()).any(|w| w == *s)
                })
            {
                break;
            }
            if Instant::now() >= deadline {
                child.kill().unwrap();
                panic!(
                    "live output missing: {} / {}",
                    String::from_utf8_lossy(&out),
                    String::from_utf8_lossy(&err)
                );
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        let lock = fs::File::open(f.root.join(".confctl/mutation.lock")).unwrap();
        assert!(
            rustix::fs::flock(&lock, rustix::fs::FlockOperation::NonBlockingLockExclusive).is_err()
        );
        assert!(child.try_wait().unwrap().is_none());
        assert!(f.root.join("active-a").exists() && f.root.join("active-b").exists());
        if cancel {
            nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(child.id() as i32),
                nix::sys::signal::Signal::SIGINT,
            )
            .unwrap();
        } else {
            fs::remove_file(f.root.join("gate")).unwrap();
        }
        let status = child.wait().unwrap();
        rustix::fs::flock(&lock, rustix::fs::FlockOperation::NonBlockingLockExclusive).unwrap();
        assert_eq!(status.success(), !cancel);
        assert!(!f.root.join("active-a").exists() && !f.root.join("active-b").exists());
        assert!(f.root.join(".confctl/mutation.lock").exists());
        if !cancel {
            let bytes = fs::read(stdout).unwrap();
            assert!(bytes.contains(&0xff));
            let text = String::from_utf8_lossy(&bytes);
            assert_eq!(text.matches("alpha> deleting").count(), 1);
            assert_eq!(text.matches("beta> deleting").count(), 1);
        }
    }
}

#[test]
fn native_gc_signal_is_aggregated_and_output_failure_stops_new_admission() {
    let f = Fixture::new();
    fs::write(f.root.join("a.signal"), b"").unwrap();
    let out = f.run(
        binary(),
        &[
            "collect-garbage",
            "--yes",
            "--max-concurrent-gc=1",
            "--tag=chosen",
        ],
        &[],
    );
    assert!(!out.status.success());
    assert!(String::from_utf8_lossy(&out.stderr).contains("signal Some(15)"));
    assert_eq!(f.calls().matches("nix-collect-garbage").count(), 2);
    let f = Fixture::new();
    let path = f.root.join("machines.json");
    let mut machines: serde_json::Value =
        serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    machines["z"] = serde_json::json!({"name":"zed","metaConfig":{"host":{"target":"z.invalid"},"tags":["chosen"]}});
    fs::write(path, serde_json::to_vec(&machines).unwrap()).unwrap();
    fs::write(f.root.join("gate"), b"").unwrap();
    let mut run = GcCase::spawn(&f, gc_command(&f), true).unwrap();
    let precondition = (|| -> io::Result<()> {
        let ready = run.ready()?;
        run.validate(&ready)?;
        run.release()
    })();
    let result = run.finish(precondition.err(), Duration::from_secs(8));
    assert!(result.errors.is_empty(), "{}", result.diagnostics);
    assert!(!result.forced, "{}", result.diagnostics);
    assert!(result.reaped, "{}", result.diagnostics);
    assert!(!result.status.unwrap().success(), "{}", result.diagnostics);
    assert!(!f.calls().contains("z.invalid"), "{}", result.diagnostics);
    assert!(
        f.root.join("gate").is_file(),
        "completion gate was released"
    );
    assert!(result.workers_gone, "{}", result.diagnostics);
    assert!(
        !f.root.join("active-a").exists() && !f.root.join("active-b").exists(),
        "{}",
        result.diagnostics
    );
    // Final availability is distinct from the exclusion observed before release.
    let lock = fs::File::open(f.root.join(".confctl/mutation.lock")).unwrap();
    rustix::fs::flock(&lock, rustix::fs::FlockOperation::NonBlockingLockExclusive).unwrap();
}

fn gc_command(f: &Fixture) -> Command {
    f.command(
        binary(),
        &[
            "collect-garbage",
            "--yes",
            "--max-concurrent-gc=2",
            "--tag=chosen",
        ],
    )
}

fn exited(fd: &OwnedFd, deadline: Instant) -> io::Result<bool> {
    loop {
        let mut polling = [rustix::event::PollFd::new(fd, rustix::event::PollFlags::IN)];
        match rustix::event::poll(
            &mut polling,
            Some(
                &rustix::event::Timespec::try_from(
                    deadline.saturating_duration_since(Instant::now()),
                )
                .map_err(io::Error::other)?,
            ),
        ) {
            Ok(count) => return Ok(count != 0),
            Err(rustix::io::Errno::INTR) if Instant::now() >= deadline => return Ok(false),
            Err(rustix::io::Errno::INTR) => continue,
            Err(error) => return Err(error.into()),
        }
    }
}

// Fixed ownership for this two-worker fixture. Every post-spawn Result and
// unexpected unwind reaches finish; no pipe drain or blocking Child::wait.
struct GcCase<'a> {
    f: &'a Fixture,
    child: Child,
    app: Option<OwnedFd>,
    workers: Vec<(i32, OwnedFd)>,
    observed_pids: Vec<i32>,
    ready_fifo: fs::File,
    releases: [fs::File; 2],
    phase: &'static str,
    status: Option<ExitStatus>,
    errors: Vec<String>,
    finish_attempted: bool,
    unresolved_hosts: Vec<&'static str>,
    observations: Vec<String>,
    child_ownership_uncertain: bool,
    failed_cancel: bool,
    fallback_release: Option<fs::File>,
}
struct GcResult {
    status: Option<ExitStatus>,
    errors: Vec<String>,
    diagnostics: String,
    forced: bool,
    reaped: bool,
    workers_gone: bool,
}
impl<'a> GcCase<'a> {
    fn spawn(f: &'a Fixture, mut command: Command, full: bool) -> io::Result<Self> {
        let fifo = |name: &str| -> io::Result<fs::File> {
            let path = f.root.join(name);
            rustix::fs::mkfifoat(
                rustix::fs::CWD,
                &path,
                rustix::fs::Mode::RUSR | rustix::fs::Mode::WUSR,
            )?;
            Ok(fs::File::from(rustix::fs::open(
                path,
                rustix::fs::OFlags::RDWR
                    | rustix::fs::OFlags::NONBLOCK
                    | rustix::fs::OFlags::CLOEXEC,
                rustix::fs::Mode::empty(),
            )?))
        };
        let ready_fifo = fifo("ready-events")?;
        let releases = [fifo("release-a")?, fifo("release-b")?];
        let stdout = if full {
            fs::File::create("/dev/full")?
        } else {
            fs::File::create(f.root.join("gc-stdout"))?
        };
        // Regular files keep a surviving worker from holding capture EOF open.
        command
            .stdin(Stdio::null())
            .stdout(stdout)
            .stderr(fs::File::create(f.root.join("gc-stderr"))?);
        let child = command.spawn()?;
        let handle = rustix::process::pidfd_open(
            rustix::process::Pid::from_child(&child),
            rustix::process::PidfdFlags::empty(),
        );
        let (app, errors) = match handle {
            Ok(fd) => (Some(fd), Vec::new()),
            Err(error) => (None, vec![format!("app pidfd: {error}")]),
        };
        Ok(Self {
            f,
            child,
            app,
            workers: Vec::new(),
            observed_pids: Vec::new(),
            ready_fifo,
            releases,
            phase: "readiness",
            status: None,
            errors,
            finish_attempted: false,
            unresolved_hosts: Vec::new(),
            observations: Vec::new(),
            child_ownership_uncertain: false,
            failed_cancel: false,
            fallback_release: None,
        })
    }
    fn ready(&mut self) -> io::Result<Vec<serde_json::Value>> {
        self.ready_records(2)
    }
    fn ready_records(&mut self, count: usize) -> io::Result<Vec<serde_json::Value>> {
        let deadline = Instant::now() + Duration::from_secs(8);
        self.observations
            .push(format!("ready FIFO deadline={deadline:?}"));
        let mut bytes = Vec::new();
        while bytes.iter().filter(|byte| **byte == b'\n').count() < count {
            let mut polling = [rustix::event::PollFd::new(
                &self.ready_fifo,
                rustix::event::PollFlags::IN,
            )];
            let count = rustix::event::poll(
                &mut polling,
                Some(
                    &rustix::event::Timespec::try_from(
                        deadline.saturating_duration_since(Instant::now()),
                    )
                    .map_err(io::Error::other)?,
                ),
            )?;
            self.status = self.child.try_wait()?;
            if count == 0 || self.status.is_some() {
                return Err(io::Error::other(format!(
                    "readiness deadline/early exit: {bytes:?}"
                )));
            }
            let mut chunk = [0; 1024];
            let n = self.ready_fifo.read(&mut chunk)?;
            bytes.extend_from_slice(&chunk[..n]);
            if bytes.len() > 2048 {
                return Err(io::Error::other("oversized readiness"));
            }
        }
        let records: Vec<serde_json::Value> = bytes
            .split(|byte| *byte == b'\n')
            .filter(|line| !line.is_empty())
            .map(|line| serde_json::from_slice(line).map_err(io::Error::other))
            .collect::<io::Result<_>>()?;
        // A complete positive FIFO record establishes entry even if the startup
        // record is later absent or malformed. Reuse pending-host accounting.
        for host in ["a", "b"] {
            if records.iter().any(|record| {
                record["host"] == host
                    && record["ready"] == true
                    && record["pid"].as_i64().is_some_and(|pid| pid > 0)
                    && record["pgid"].as_i64().is_some_and(|pgid| pgid > 0)
            }) && !self.unresolved_hosts.contains(&host)
            {
                self.unresolved_hosts.push(host);
            }
        }
        Ok(records)
    }
    fn validate(&mut self, ready: &[serde_json::Value]) -> io::Result<()> {
        self.phase = "readiness validation";
        if self.app.is_none() {
            return Err(io::Error::other("app pidfd unavailable"));
        }
        self.track_workers()?;
        if self.workers.len() != 2 {
            return Err(io::Error::other("two bound worker pidfds required"));
        }
        let mut ready = ready.to_vec();
        ready.sort_by_key(|record| record["host"].as_str().unwrap_or("").to_owned());
        if ready.len() != 2 || ready[0]["pid"] == ready[1]["pid"] {
            return Err(io::Error::other("expected two distinct ready workers"));
        }
        for (host, record) in ["a", "b"].into_iter().zip(&ready) {
            let pid = record["pid"]
                .as_i64()
                .and_then(|p| i32::try_from(p).ok())
                .filter(|p| *p > 0);
            let pgid = record["pgid"].as_i64().filter(|p| *p > 0);
            if record["host"] != host || record["ready"] != true || pid.is_none() || pgid.is_none()
            {
                return Err(io::Error::other(format!("invalid ready-{host}: {record}")));
            }
            let observed = nix::unistd::getpgid(Some(nix::unistd::Pid::from_raw(pid.unwrap())))
                .map_err(io::Error::other)?
                .as_raw() as i64;
            let saved: serde_json::Value =
                serde_json::from_slice(&fs::read(self.f.root.join(format!("ready-{host}")))?)
                    .map_err(io::Error::other)?;
            if Some(observed) != pgid
                || !self.f.root.join(format!("active-{host}")).is_file()
                || saved != *record
            {
                return Err(io::Error::other(format!(
                    "worker readiness mismatch: {record}"
                )));
            }
        }
        let lock = fs::File::open(self.f.root.join(".confctl/mutation.lock"))?;
        if rustix::fs::flock(&lock, rustix::fs::FlockOperation::NonBlockingLockExclusive)
            != Err(rustix::io::Errno::WOULDBLOCK)
        {
            return Err(io::Error::other("mutation lock not held before release"));
        }
        self.status = self.child.try_wait()?;
        if self.status.is_some() {
            return Err(io::Error::other("app exited before release"));
        }
        Ok(())
    }
    fn release(&mut self) -> io::Result<()> {
        self.phase = "release";
        if self.app.is_none() || self.workers.len() != 2 {
            return Err(io::Error::other(
                "release requires app and both bound worker pidfds",
            ));
        }
        for endpoint in &mut self.releases {
            endpoint.write_all(b"1")?;
        }
        self.phase = "completion";
        Ok(())
    }
    fn track_workers(&mut self) -> io::Result<()> {
        let mut errors = Vec::new();
        for host in ["a", "b"] {
            if let Err(error) = self.track_worker(host) {
                errors.push(format!("{host}: {error}"));
            }
        }
        if errors.is_empty() {
            Ok(())
        } else {
            Err(io::Error::other(errors.join("; ")))
        }
    }
    fn track_worker(&mut self, host: &'static str) -> io::Result<()> {
        let path = self.f.root.join(format!("started-{host}"));
        // Record entry evidence before any fallible read, decode or PID step.
        // Plain absence before entry is not evidence of an actor.
        let entry_seen = match fs::symlink_metadata(&path) {
            Ok(_) => true,
            Err(error) => error.kind() != io::ErrorKind::NotFound,
        };
        if (entry_seen || self.f.root.join(format!("ready-{host}")).exists())
            && !self.unresolved_hosts.contains(&host)
        {
            self.unresolved_hosts.push(host);
        }
        let bytes = match fs::read(path) {
            Ok(bytes) => bytes,
            Err(e)
                if e.kind() == io::ErrorKind::NotFound
                    && !self.unresolved_hosts.contains(&host) =>
            {
                return Ok(());
            }
            Err(e) => return Err(e),
        };
        let record: serde_json::Value = serde_json::from_slice(&bytes).map_err(io::Error::other)?;
        let pid = record["pid"]
            .as_i64()
            .and_then(|p| i32::try_from(p).ok())
            .filter(|p| *p > 0)
            .ok_or_else(|| io::Error::other("invalid startup PID"))?;
        // Observed PIDs are diagnostic candidates, not retained ownership.
        // Failed pidfd acquisition/validation must remain retryable.
        if !self.observed_pids.contains(&pid) {
            self.observed_pids.push(pid);
        }
        if self.workers.iter().any(|(retained, _)| *retained == pid) {
            self.unresolved_hosts.retain(|pending| *pending != host);
            return Ok(());
        }
        let fd = match rustix::process::pidfd_open(
            rustix::process::Pid::from_raw(pid).ok_or_else(|| io::Error::other("invalid PID"))?,
            rustix::process::PidfdFlags::empty(),
        ) {
            Ok(fd) => fd,
            Err(rustix::io::Errno::SRCH) => return Ok(()),
            Err(e) => return Err(e.into()),
        };
        if exited(&fd, Instant::now())? {
            return Ok(());
        }
        let proc = format!("/proc/{pid}");
        let cmdline = fs::read(format!("{proc}/cmdline"))?;
        let args = cmdline
            .split(|byte| *byte == 0)
            .filter(|arg| !arg.is_empty())
            .collect::<Vec<_>>();
        let script = self.f.bin.join("ssh").canonicalize()?;
        let cwd = fs::read_link(format!("{proc}/cwd"))?;
        // pidfd opened first; validate this unique fixture's exact GC script
        // invocation and canonical cwd while that same handle is still live.
        let valid = args.get(1) == Some(&script.as_os_str().as_bytes())
            && args.contains(&format!("{host}.invalid").as_bytes())
            && args.last() == Some(&b"nix-collect-garbage".as_slice())
            && cwd == self.f.root.canonicalize()?
            && record["host"] == host;
        if exited(&fd, Instant::now())? {
            return Ok(());
        }
        if !valid {
            return Err(io::Error::other(format!(
                "unverified fixture worker {pid}: {cmdline:?}"
            )));
        }
        self.workers.push((pid, fd));
        self.unresolved_hosts.retain(|pending| *pending != host);
        self.observations.push(format!("bound {host} pid={pid}"));
        Ok(())
    }
    fn error(&mut self, text: String) {
        if !self.errors.contains(&text) {
            self.errors.push(text);
        }
    }
    fn observe(&mut self) {
        if let Err(error) = self.track_workers() {
            self.error(format!("discovery: {error}"));
        }
        if self.status.is_none() && !self.child_ownership_uncertain {
            match self.child.try_wait() {
                Ok(status) => self.status = status,
                Err(error) => {
                    // Do not numerically signal after exclusive wait ownership
                    // becomes uncertain (in particular, unexplained ECHILD).
                    self.child_ownership_uncertain = true;
                    self.error(format!("app wait ownership: {error}"));
                }
            }
        }
    }
    fn pause(&self, deadline: Instant) -> io::Result<()> {
        let remaining = deadline
            .saturating_duration_since(Instant::now())
            .min(Duration::from_millis(20));
        if remaining.is_zero() {
            return Ok(());
        }
        let timeout = rustix::event::Timespec::try_from(remaining).map_err(io::Error::other)?;
        // Full bounded fallback grace even if an app pidfd could not be opened.
        match rustix::event::poll(&mut [], Some(&timeout)) {
            Ok(_) | Err(rustix::io::Errno::INTR) => Ok(()),
            Err(error) => Err(error.into()),
        }
    }
    fn wait_app(&mut self, deadline: Instant) -> io::Result<bool> {
        self.observations.push(format!(
            "app wait phase={} deadline={deadline:?}",
            self.phase
        ));
        loop {
            self.observe();
            if self.status.is_some() {
                return Ok(true);
            }
            if self.child_ownership_uncertain || Instant::now() >= deadline {
                return Ok(false);
            }
            self.pause(deadline)?;
            // A fixed direct-child regression releases its native FIFO only
            // after a real fallback wait slice. No scheduling/race assumption.
            if self.app.is_none()
                && let Some(mut release) = self.fallback_release.take()
            {
                release.write_all(b"1")?;
                self.observations
                    .push("direct-child fallback release after wait slice".into());
            }
        }
    }
    fn cancel_app(&mut self) -> io::Result<()> {
        if self.failed_cancel {
            return Err(io::Error::other("injected failed-cancel boundary"));
        }
        self.signal_app(rustix::process::Signal::INT)
    }
    fn signal_app(&mut self, signal: rustix::process::Signal) -> io::Result<()> {
        if self.status.is_some() {
            return Ok(());
        }
        if self.child_ownership_uncertain {
            return Err(io::Error::other(
                "app wait ownership uncertain; no numeric signal",
            ));
        }
        let result = if let Some(fd) = &self.app {
            rustix::process::pidfd_send_signal(fd, signal).map_err(io::Error::from)
        } else {
            nix::sys::signal::kill(
                nix::unistd::Pid::from_raw(self.child.id() as i32),
                nix::sys::signal::Signal::try_from(signal.as_raw()).map_err(io::Error::other)?,
            )
            .map_err(io::Error::other)
        };
        self.observations
            .push(format!("app signal {signal:?}: {result:?}"));
        result
    }
    fn terminal_workers(&mut self) -> bool {
        let mut terminal = self.unresolved_hosts.is_empty();
        let mut notes = Vec::new();
        let mut errors = Vec::new();
        for (pid, fd) in &self.workers {
            let exited = exited(fd, Instant::now());
            let stat = fs::metadata(format!("/proc/{pid}"));
            let absent = matches!(&stat, Err(error) if error.kind() == io::ErrorKind::NotFound);
            terminal &= matches!(exited, Ok(true)) && absent;
            notes.push(format!(
                "worker {pid} terminal={exited:?} proc_absent={absent}"
            ));
            if let Err(error) = exited {
                errors.push(format!("worker {pid} terminal poll: {error}"));
            }
            if let Err(error) = stat
                && error.kind() != io::ErrorKind::NotFound
            {
                errors.push(format!("worker {pid} proc status: {error}"));
            }
        }
        for error in errors {
            self.error(error);
        }
        for note in notes {
            if !self.observations.contains(&note) {
                self.observations.push(note);
            }
        }
        terminal
    }
    fn escalate_workers(
        &mut self,
        signal: rustix::process::Signal,
        deadline: Instant,
    ) -> io::Result<bool> {
        self.observations
            .push(format!("worker {signal:?} deadline={deadline:?}"));
        let mut signalled = Vec::new();
        loop {
            self.observe(); // Admissions/records remain discoverable while app lives.
            for (pid, fd) in &self.workers {
                if !signalled.contains(pid) && !exited(fd, Instant::now())? {
                    let result = rustix::process::pidfd_send_signal(fd, signal);
                    self.observations
                        .push(format!("worker {pid} {signal:?}: {result:?}"));
                    signalled.push(*pid);
                    if let Err(error) = result {
                        self.errors.push(format!("worker {pid} signal: {error}"));
                    }
                }
            }
            if self.status.is_some() && self.terminal_workers() {
                return Ok(true);
            }
            if Instant::now() >= deadline {
                return Ok(false);
            }
            self.pause(deadline)?;
        }
    }
    fn record_wait(&mut self, phase: &str, result: io::Result<bool>) -> bool {
        self.observations.push(format!("{phase}: {result:?}"));
        match result {
            Ok(true) => true,
            Ok(false) => {
                self.error(format!("{phase} deadline expired"));
                false
            }
            Err(error) => {
                self.error(format!("{phase}: {error}"));
                false
            }
        }
    }
    fn finish(&mut self, failure: Option<io::Error>, budget: Duration) -> GcResult {
        self.finish_attempted = true; // Attempted is explicitly not terminal proof.
        if let Some(error) = failure {
            self.error(format!("{}: {error}", self.phase));
        }
        self.observe();
        if self.app.is_none() {
            self.error("app pidfd unavailable".into());
        }
        if self.errors.is_empty() {
            let result = self.wait_app(Instant::now() + budget);
            let phase = format!("{} timeout", self.phase);
            self.record_wait(&phase, result);
        }
        if !self.errors.is_empty() && self.status.is_none() {
            if let Err(error) = self.cancel_app() {
                self.error(format!("cancel app: {error}"));
            }
            let result = self.wait_app(Instant::now() + Duration::from_secs(8));
            self.record_wait("cancel grace", result);
        }
        self.observe();
        let forced = self.status.is_none() || !self.terminal_workers();
        let mut diagnostics = self.evidence();
        if forced {
            self.error("forced cleanup required; this is not a Supervisor join".into());
            for signal in [rustix::process::Signal::TERM, rustix::process::Signal::KILL] {
                let result = self.escalate_workers(signal, Instant::now() + Duration::from_secs(1));
                if self.record_wait(&format!("worker {signal:?} grace"), result) {
                    break;
                }
            }
            // Preserve app responsibility for wait/reap before app escalation.
            let result = self.wait_app(Instant::now() + Duration::from_secs(2));
            if !self.record_wait("app worker-reap grace", result) {
                if let Err(error) = self.signal_app(rustix::process::Signal::KILL) {
                    self.error(format!("app kill: {error}"));
                }
                let result = self.wait_app(Instant::now() + Duration::from_secs(2));
                self.record_wait("final app reap", result);
            }
        }
        // A fixed final interval discovers late records even after app exit.
        // New validated actors receive KILL only in an already-forced case.
        let deadline = Instant::now() + Duration::from_millis(200);
        self.observations
            .push(format!("final discovery deadline={deadline:?}"));
        let result = loop {
            self.observe();
            if forced {
                // Do not end the final discovery interval early when already
                // known handles are terminal: another record can arrive later.
                if let Err(error) =
                    self.escalate_workers(rustix::process::Signal::KILL, Instant::now())
                {
                    break Err(error);
                }
            }
            if Instant::now() >= deadline {
                break Ok(self.status.is_some() && self.terminal_workers());
            }
            if let Err(error) = self.pause(deadline) {
                break Err(error);
            }
        };
        self.record_wait("final discovery/terminal evidence", result);
        self.observe(); // Last scan never implies successful binding by itself.
        let workers_gone = self.terminal_workers();
        let reaped = self.status.is_some() && !self.child_ownership_uncertain;
        let complete = workers_gone && reaped;
        diagnostics.push_str(&format!("final evidence:\n{}", self.evidence()));
        diagnostics.push_str(&format!("root={} bin={} phase={} appPID={} errors={:?} observations={:?} unresolved_hosts={:?} forced={forced} reaped={reaped} workers_gone={workers_gone} cleanup_complete={complete}\n",
            self.f.root.display(), self.f.bin.display(), self.phase, self.child.id(), self.errors, self.observations, self.unresolved_hosts));
        for observation in &self.observations {
            diagnostics.push_str(&format!("observation: {observation}\n"));
        }
        if !complete {
            self.error("HARD cleanup incomplete; terminal state unresolved, handle release is not transfer/reap".into());
            let retained = self.f.preserve();
            diagnostics.push_str(&format!("HARD cleanup incomplete; retained={retained:?}; errors={:?}; local handles will close; STOP further campaign/retries and report root\n", self.errors));
            if let Err(error) = fs::write(self.f.root.join("cleanup-incomplete.txt"), &diagnostics)
            {
                diagnostics.push_str(&format!(
                    "failed to record incomplete-cleanup evidence: {error}\n"
                ));
            }
        }
        // Preserve successful fault-case receipts as well as failed diagnostics.
        if let Err(error) = fs::write(self.f.root.join("cleanup-attempt.txt"), &diagnostics) {
            let failure = format!("record cleanup diagnostics: {error}");
            diagnostics.push_str(&format!("{failure}\n"));
            self.error(failure);
        }
        let _ = io::stderr().write_all(diagnostics.as_bytes());
        GcResult {
            status: self.status,
            errors: self.errors.clone(),
            diagnostics,
            forced,
            reaped,
            workers_gone,
        }
    }
    fn evidence(&self) -> String {
        let stderr = fs::read(self.f.root.join("gc-stderr")).unwrap_or_default();
        gc_diagnostics(self.f, self.status, &stderr)
    }
}
impl Drop for GcCase<'_> {
    fn drop(&mut self) {
        if !self.finish_attempted {
            self.finish(
                Some(io::Error::other("unexpected unwind before fixture finish")),
                Duration::ZERO,
            );
        }
    }
}

#[test]
fn rejected_readiness_and_completion_timeout_cancel_and_reap_owned_fixture_actors() {
    for reject in [true, false] {
        let f = Fixture::new();
        fs::write(f.root.join("gate"), b"").unwrap();
        let mut run = GcCase::spawn(&f, gc_command(&f), false).unwrap();
        let precondition = (|| -> io::Result<()> {
            let mut ready = run.ready()?;
            run.validate(&ready)?; // Positively establish both actual actors.
            if reject {
                ready[0]["ready"] = false.into();
                run.validate(&ready)?;
            }
            run.release()
        })();
        let result = run.finish(precondition.err(), Duration::ZERO);
        assert!(
            result.reaped && result.workers_gone && !result.forced,
            "{}",
            result.diagnostics
        );
        assert!(
            result.errors.iter().any(|e| e.contains(if reject {
                "invalid ready"
            } else {
                "completion timeout"
            })),
            "{}",
            result.diagnostics
        );
        assert!(!result.status.unwrap().success(), "{}", result.diagnostics);
        assert!(f.root.join("gate").is_file());
        assert!(!f.root.join("active-a").exists() && !f.root.join("active-b").exists());
        assert_eq!(run.observed_pids.len(), 2);
        for host in ["a", "b"] {
            assert!(f.root.join(format!("stopped-{host}")).is_file());
        }
        assert!(result.diagnostics.contains("ready-a=") && result.diagnostics.contains("ready-b="));
    }
}

#[test]
fn incomplete_discovery_retries_binding_keeps_peer_and_finds_later_record() {
    for mode in [
        "wrong-host",
        "later-b",
        "incomplete-json",
        "invalid-pid",
        "known-ready-missing",
    ] {
        let f = Fixture::new();
        fs::write(f.root.join("gate"), b"").unwrap();
        let mut run = GcCase::spawn(&f, gc_command(&f), false).unwrap();
        let precondition = (|| -> io::Result<()> {
            let ready = run.ready()?; // Both actual actors entered, no binding yet.
            if ready.len() != 2 || !run.workers.is_empty() {
                return Err(io::Error::other("discovery fixture precondition"));
            }
            let target = if mode == "later-b" { "b" } else { "a" };
            let path = f.root.join(format!("started-{target}"));
            let original = fs::read(&path)?;
            match mode {
                "wrong-host" | "invalid-pid" => {
                    let mut record: serde_json::Value = serde_json::from_slice(&original)?;
                    if mode == "wrong-host" {
                        record["host"] = "wrong-host".into();
                    } else {
                        record["pid"] = 0.into();
                    }
                    fs::write(&path, serde_json::to_vec(&record)?)?;
                }
                "incomplete-json" => fs::write(&path, b"{")?,
                _ => fs::rename(&path, f.root.join("later-startup"))?,
            }
            let first = run.track_workers();
            let retained = run.workers.iter().map(|(pid, _)| *pid).collect::<Vec<_>>();
            let pending = run.unresolved_hosts.contains(&target);
            let premature_terminal = run.terminal_workers();
            // Restore REAL records before interpreting results or asserting.
            if matches!(mode, "later-b" | "known-ready-missing") {
                fs::rename(f.root.join("later-startup"), &path)?;
            } else {
                fs::write(&path, original)?;
            }
            let peer = if target == "a" { "b" } else { "a" };
            let peer_pid = ready
                .iter()
                .find(|record| record["host"] == peer)
                .and_then(|record| record["pid"].as_i64())
                .ok_or_else(|| io::Error::other("missing genuine ready PID"))?
                as i32;
            if retained != [peer_pid] || !pending || premature_terminal {
                return Err(io::Error::other(format!(
                    "pending/peer accounting failed for {mode}: retained={retained:?} expected={peer_pid} pending={pending} terminal={premature_terminal}"
                )));
            }
            let original_error = first
                .err()
                .ok_or_else(|| io::Error::other("incomplete startup observation was accepted"))?;
            run.track_workers()?;
            if run.workers.len() != 2 || !run.unresolved_hosts.is_empty() {
                return Err(io::Error::other("incomplete binding was not retried"));
            }
            run.validate(&ready)?;
            Err(io::Error::other(format!(
                "injected {mode}: {original_error}"
            )))
        })();
        let result = run.finish(precondition.err(), Duration::ZERO);
        assert!(
            result.reaped && result.workers_gone && !result.forced,
            "{}",
            result.diagnostics
        );
        assert!(
            result
                .errors
                .iter()
                .any(|error| error.contains(&format!("injected {mode}:"))),
            "{}",
            result.diagnostics
        );
        assert_eq!(run.workers.len(), 2, "{}", result.diagnostics);
        assert!(f.root.join("gate").is_file());
        assert!(!f.root.join("active-a").exists() && !f.root.join("active-b").exists());
        assert!(!result.status.unwrap().success());
    }
}

#[test]
fn forced_cleanup_records_term_expiry_real_pidfd_kill_and_terminal_reap() {
    let f = Fixture::new();
    fs::write(f.root.join("gate"), b"").unwrap();
    fs::write(f.root.join("ignore-term"), b"").unwrap();
    let mut run = GcCase::spawn(&f, gc_command(&f), false).unwrap();
    let precondition = (|| -> io::Result<()> {
        let ready = run.ready()?;
        run.validate(&ready)?;
        run.release()?;
        run.failed_cancel = true; // Explicit boundary injection, no timing race.
        Err(io::Error::other("injected failed-cancel case"))
    })();
    let result = run.finish(precondition.err(), Duration::ZERO);
    assert!(
        result.forced && result.reaped && result.workers_gone,
        "{}",
        result.diagnostics
    );
    assert!(
        result
            .errors
            .iter()
            .any(|e| e.contains("injected failed-cancel boundary")),
        "{}",
        result.diagnostics
    );
    assert!(
        result.errors.iter().any(|e| e.contains(&format!(
            "{:?} grace deadline expired",
            rustix::process::Signal::TERM
        ))),
        "{}",
        result.diagnostics
    );
    for (pid, _) in &run.workers {
        assert!(
            result.diagnostics.contains(&format!(
                "worker {pid} {:?}: Ok",
                rustix::process::Signal::KILL
            )),
            "{}",
            result.diagnostics
        );
    }
    for host in ["a", "b"] {
        assert!(f.root.join(format!("ignored-term-{host}")).is_file());
        // SIGKILL cannot perform Python handler cleanup; preserve honest markers.
        assert!(f.root.join(format!("active-{host}")).is_file());
        assert!(!f.root.join(format!("stopped-{host}")).exists());
    }
    assert!(f.root.join("gate").is_file());
    assert!(!result.status.unwrap().success());
}

#[test]
fn missing_app_pidfd_uses_full_direct_child_wait_grace_and_preserves_failure() {
    use std::os::unix::process::CommandExt;
    let f = Fixture::new();
    let path = f.root.join("direct-wait-release");
    rustix::fs::mkfifoat(
        rustix::fs::CWD,
        &path,
        rustix::fs::Mode::RUSR | rustix::fs::Mode::WUSR,
    )
    .unwrap();
    let release = fs::File::from(
        rustix::fs::open(
            &path,
            rustix::fs::OFlags::RDWR | rustix::fs::OFlags::NONBLOCK | rustix::fs::OFlags::CLOEXEC,
            rustix::fs::Mode::empty(),
        )
        .unwrap(),
    );
    let mut command = f.command(&f.bin.join("nix-collect-garbage"), &[]);
    command.process_group(0);
    let mut run = GcCase::spawn(&f, command, false).unwrap();
    let precondition = (|| -> io::Result<()> {
        let ready = run.ready_records(1)?;
        if ready.len() != 1 || ready[0]["pid"] != run.child.id() || ready[0]["host"] != "local" {
            return Err(io::Error::other("direct-child readiness identity"));
        }
        run.app.take(); // Model the explicitly failed pidfd precondition.
        if run.wait_app(Instant::now())? {
            return Err(io::Error::other("direct child escaped FIFO phase"));
        }
        run.fallback_release = Some(release);
        if !run.wait_app(Instant::now() + Duration::from_secs(8))? {
            return Err(io::Error::other("direct child fallback grace failed"));
        }
        Err(io::Error::other("injected app pidfd unavailable"))
    })();
    let result = run.finish(precondition.err(), Duration::ZERO);
    assert!(
        result.reaped && result.workers_gone && !result.forced,
        "{}",
        result.diagnostics
    );
    assert!(
        result
            .errors
            .iter()
            .any(|e| e.contains("app pidfd unavailable"))
    );
    assert!(
        result
            .diagnostics
            .contains("direct-child fallback release after wait slice")
    );
    assert_eq!(result.status.unwrap().code(), Some(0));
    assert!(!f.root.join("active-local").exists());
    assert!(
        matches!(fs::metadata(format!("/proc/{}", run.child.id())), Err(e) if e.kind() == io::ErrorKind::NotFound)
    );
}

// Collect before outcome assertions, so a stale marker is distinguishable from
// a live/zombie child. This is evidence about recorded PIDs, not a join claim.
fn gc_diagnostics(f: &Fixture, status: Option<ExitStatus>, stderr: &[u8]) -> String {
    let mut text = format!(
        "app={:?} code={:?} signal={:?} stderr={:?} calls={}\n",
        status,
        status.and_then(|status| status.code()),
        status.and_then(|status| status.signal()),
        stderr,
        f.calls()
    );
    for host in ["a", "b", "local"] {
        for phase in ["started", "ready", "stopped", "active"] {
            let bytes = fs::read(f.root.join(format!("{phase}-{host}")))
                .map(|bytes| String::from_utf8_lossy(&bytes).into_owned());
            text.push_str(&format!("{phase}-{host}={bytes:?}\n"));
        }
        let record = fs::read(f.root.join(format!("started-{host}")))
            .ok()
            .and_then(|bytes| serde_json::from_slice::<serde_json::Value>(&bytes).ok());
        if let Some(pid) = record.and_then(|record| record["pid"].as_u64()) {
            let proc_status = fs::File::open(format!("/proc/{pid}/status")).and_then(|file| {
                let mut bytes = Vec::new();
                file.take(4096).read_to_end(&mut bytes)?;
                Ok(String::from_utf8_lossy(&bytes).into_owned())
            });
            text.push_str(&format!("proc-{host}-{pid}={proc_status:?}\n"));
        }
    }
    text
}

#[test]
fn pending_term_during_gc_startup_is_handled_and_direct_child_is_reaped() {
    use std::os::unix::process::CommandExt;
    let f = Fixture::new();
    fs::write(f.root.join("startup-term"), b"").unwrap();
    let mut command = f.command(&f.bin.join("nix-collect-garbage"), &[]);
    command.process_group(0);
    let mut run = GcCase::spawn(&f, command, false).unwrap();
    let pid = nix::unistd::Pid::from_raw(run.child.id().try_into().unwrap());
    let result = run.finish(None, Duration::from_secs(8));
    let diagnostics = &result.diagnostics;
    assert!(
        result.errors.is_empty() && result.reaped && !result.forced,
        "{diagnostics}"
    );
    let started: serde_json::Value =
        serde_json::from_slice(&fs::read(f.root.join("started-local")).unwrap()).unwrap();
    let stopped: serde_json::Value =
        serde_json::from_slice(&fs::read(f.root.join("stopped-local")).unwrap()).unwrap();
    assert_eq!(started["pid"], pid.as_raw());
    assert_eq!(started["pgid"], pid.as_raw());
    assert_eq!(started["pending_term"], true);
    assert_eq!(stopped["pid"], started["pid"]);
    assert_eq!(stopped["signal"], 15);
    assert_eq!(result.status.unwrap().code(), Some(143), "{diagnostics}");
    assert_eq!(result.status.unwrap().signal(), None);
    assert!(fs::read(f.root.join("gc-stdout")).unwrap().is_empty());
    assert!(!f.root.join("active-local").exists());
    assert!(!f.root.join("ready-local").exists());
    assert_eq!(
        nix::sys::wait::waitpid(pid, Some(nix::sys::wait::WaitPidFlag::WNOHANG)),
        Err(nix::errno::Errno::ECHILD)
    );
}
