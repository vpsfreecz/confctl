//! The rollback watchdog deliberately outlives the operator transport. Its
//! private self-reexec inherits the parent's process group so that the worker
//! can call setsid before creating threads or touching deployment state.
//! Switch children have one owner and both pipes are drained even after the
//! original transport output breaks. Native nonzero switch status is ignored,
//! preserving the original helper's confirmation/rollback protocol.

use std::{
    ffi::{OsStr, OsString},
    io::{self, Read, Write},
    path::{Path, PathBuf},
    process::{Command, ExitStatus, Stdio},
    time::{Duration, SystemTime},
};

const WORKER: &str = "--__confctl-auto-rollback-worker";

struct WorkerFailure {
    primary: io::Error,
    secondary: Vec<io::Error>,
}

impl std::fmt::Debug for WorkerFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("WorkerFailure")
            .field("primary_kind", &self.primary.kind())
            .field(
                "secondary_kinds",
                &self
                    .secondary
                    .iter()
                    .map(io::Error::kind)
                    .collect::<Vec<_>>(),
            )
            .finish_non_exhaustive()
    }
}

impl std::fmt::Display for WorkerFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.primary)?;
        for error in &self.secondary {
            write!(f, "; secondary: {error}")?;
        }
        Ok(())
    }
}

impl std::error::Error for WorkerFailure {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(&self.primary)
    }
}

#[derive(Debug, PartialEq, Eq)]
struct Timeout {
    negative: bool,
    decimal: String,
}

impl Timeout {
    fn parse(text: &str) -> io::Result<Self> {
        // OptionParser's Integer converter is narrower than Kernel.Integer:
        // it rejects surrounding whitespace and explicit 0o/0d prefixes.
        let unsigned = text.strip_prefix(['+', '-']).unwrap_or(text);
        if text.trim_matches(['\t', '\n', '\x0b', '\x0c', '\r', ' ']) != text
            || ["0o", "0O", "0d", "0D"]
                .iter()
                .any(|prefix| unsigned.starts_with(*prefix))
        {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("invalid argument: --timeout {text}"),
            ));
        }
        super::ruby_integer::canonical(text)
            .map(|(negative, decimal)| Self { negative, decimal })
            .ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!("invalid argument: --timeout {text}"),
                )
            })
    }

    fn expired(&self, start: SystemTime, now: SystemTime) -> bool {
        let seconds = self.decimal.parse::<u64>();
        match (self.negative, now.duration_since(start), seconds) {
            (false, Ok(elapsed), Ok(seconds)) => elapsed > Duration::from_secs(seconds),
            (false, _, _) => false,
            (true, Ok(_), _) => true,
            (true, Err(backwards), Ok(seconds)) => {
                backwards.duration() < Duration::from_secs(seconds)
            }
            (true, Err(_), Err(_)) => true,
        }
    }

    fn display(&self) -> String {
        format!("{}{}", if self.negative { "-" } else { "" }, self.decimal)
    }
}

struct Options {
    timeout: Timeout,
    toplevel: PathBuf,
    action: OsString,
    check_file: PathBuf,
}

enum Parsed {
    Run(Options),
    Help,
    Invalid,
}

fn parse(args: &[OsString]) -> io::Result<Parsed> {
    let mut timeout = Timeout::parse("60")?;
    let mut positional = Vec::new();
    let mut args = args.iter();
    while let Some(arg) = args.next() {
        let text = arg.to_str().unwrap_or("");
        if text == "--" {
            positional.extend(args.cloned());
            break;
        }
        if matches!(text, "-h" | "--help") {
            return Ok(Parsed::Help);
        }
        let value = if text == "-t" || text == "--timeout" {
            Some(
                args.next()
                    .ok_or_else(|| {
                        io::Error::new(io::ErrorKind::InvalidInput, "missing argument: --timeout")
                    })?
                    .to_str()
                    .ok_or_else(|| {
                        io::Error::new(io::ErrorKind::InvalidInput, "invalid timeout encoding")
                    })?,
            )
        } else if let Some(value) = text.strip_prefix("--timeout=") {
            Some(value)
        } else if text.starts_with("-t") && !text.starts_with("--") && text.len() > 2 {
            Some(&text[2..])
        } else {
            None
        };
        if let Some(value) = value {
            timeout = Timeout::parse(value)?;
        } else if text.starts_with('-') {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("invalid option: {text}"),
            ));
        } else {
            positional.push(arg.clone());
        }
    }
    if positional.len() != 3 {
        return Ok(Parsed::Invalid);
    }
    Ok(Parsed::Run(Options {
        timeout,
        toplevel: positional[0].clone().into(),
        action: positional[1].clone(),
        check_file: positional[2].clone().into(),
    }))
}

fn help(program: &OsStr) -> String {
    format!(
        "Usage: {} [options] <toplevel> <action> <check file>\n    -t, --timeout TIMEOUT            Timeout in seconds\n    -h, --help                       Print help message and exit\n",
        program.to_string_lossy()
    )
}

fn write_transport(writer: &mut impl Write, bytes: &[u8]) -> io::Result<()> {
    match writer.write_all(bytes).and_then(|()| writer.flush()) {
        Err(error) if error.kind() == io::ErrorKind::BrokenPipe => Ok(()),
        result => result,
    }
}

// A transport failure must never close a switch child's pipe early. Continue
// reading to EOF with bounded memory, retaining non-EPIPE infrastructure errors.
fn drain(mut reader: impl Read, mut writer: impl Write) -> io::Result<()> {
    let mut failures = Vec::new();
    let mut writable = true;
    let mut buffer = [0u8; 16 * 1024];
    loop {
        let count = match reader.read(&mut buffer) {
            Ok(0) => break,
            Ok(count) => count,
            Err(error) if error.kind() == io::ErrorKind::Interrupted => continue,
            Err(error) => {
                failures.push(error);
                break;
            }
        };
        if writable && let Err(error) = writer.write_all(&buffer[..count]) {
            writable = false;
            if error.kind() != io::ErrorKind::BrokenPipe {
                failures.push(error);
            }
        }
    }
    if writable
        && let Err(error) = writer.flush()
        && error.kind() != io::ErrorKind::BrokenPipe
    {
        failures.push(error);
    }
    final_result(0, failures).map(|_| ())
}

fn switch(system: &Path, action: &OsStr) -> io::Result<Vec<io::Error>> {
    let mut child = Command::new(system.join("bin/switch-to-configuration"))
        .arg(action)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()?;
    let stdout = child.stdout.take().expect("piped stdout");
    let stderr = child.stderr.take().expect("piped stderr");
    std::thread::scope(|scope| {
        let out = scope.spawn(move || drain(stdout, io::stdout().lock()));
        let err = scope.spawn(move || drain(stderr, io::stderr().lock()));
        // One final wait, irrespective of pipe errors or native child status.
        let status = child.wait();
        let out = out
            .join()
            .unwrap_or_else(|_| Err(io::Error::other("switch stdout drainer panicked")));
        let err = err
            .join()
            .unwrap_or_else(|_| Err(io::Error::other("switch stderr drainer panicked")));
        let mut errors: Vec<_> = [out.err(), err.err()].into_iter().flatten().collect();
        match status {
            Ok(status) => {
                #[cfg(test)]
                if let Err(error) = fixture_wait("switch", child.id(), status) {
                    errors.push(error);
                }
                #[cfg(not(test))]
                let _ = status;
            }
            Err(error) => {
                errors.insert(0, error);
                return final_result(1, errors).map(|_| Vec::new());
            }
        }
        Ok(errors)
    })
}

fn final_result(code: i32, errors: Vec<io::Error>) -> io::Result<i32> {
    if errors.is_empty() {
        Ok(code)
    } else {
        let mut errors = errors.into_iter();
        let primary = errors.next().expect("nonempty worker failures");
        Err(io::Error::new(
            primary.kind(),
            WorkerFailure {
                primary,
                secondary: errors.collect(),
            },
        ))
    }
}

fn report(output: &mut impl Write, failures: &mut Vec<io::Error>, text: &str) {
    if let Err(error) = write_transport(output, text.as_bytes()) {
        failures.push(error);
    }
}

fn worker(options: Options, current_system: &Path) -> io::Result<i32> {
    let old = std::fs::read_link(current_system)?;
    let mut output = io::stdout();
    let mut failures = Vec::new();
    let outcome = (|| -> io::Result<i32> {
        report(
            &mut output,
            &mut failures,
            &format!(
                "  current system = {}\n  new system     = {}\n  action         = {}\n  check file     = {}\n  timeout        = {} seconds\n\n",
                old.display(),
                options.toplevel.display(),
                options.action.to_string_lossy(),
                options.check_file.display(),
                options.timeout.display()
            ),
        );
        report(
            &mut output,
            &mut failures,
            "Switching to new configuration\n",
        );
        std::fs::write(&options.check_file, b"switching")?;
        let initial_errors = switch(&options.toplevel, &options.action)?;
        failures.extend(initial_errors);
        std::fs::write(&options.check_file, b"switched")?;
        report(
            &mut output,
            &mut failures,
            "Switch complete, waiting for confirmation\n",
        );
        let start = SystemTime::now();
        loop {
            std::thread::sleep(Duration::from_millis(500));
            let content = std::fs::read(&options.check_file)?;
            let whitespace =
                |byte: &u8| matches!(*byte, 0 | b'\t' | b'\n' | b'\x0b' | b'\x0c' | b'\r' | b' ');
            let first = content
                .iter()
                .position(|byte| !whitespace(byte))
                .unwrap_or(content.len());
            let last = content
                .iter()
                .rposition(|byte| !whitespace(byte))
                .map_or(first, |index| index + 1);
            if &content[first..last] == b"confirmed" {
                report(&mut output, &mut failures, "Configuration confirmed\n");
                std::fs::remove_file(&options.check_file)?;
                return Ok(0);
            }
            if options.timeout.expired(start, SystemTime::now()) {
                break;
            }
        }
        report(
            &mut output,
            &mut failures,
            "Timeout occurred, rolling back\n",
        );
        let rollback_errors = switch(&old, &options.action)?;
        failures.extend(rollback_errors);
        report(&mut output, &mut failures, "Rollback complete\n");
        Ok(1)
    })();
    match outcome {
        Ok(code) => final_result(code, failures),
        Err(error) => {
            failures.push(error);
            final_result(1, failures)
        }
    }
}

fn mirror(status: ExitStatus) -> i32 {
    use std::os::unix::process::ExitStatusExt;
    if let Some(code) = status.code() {
        return code;
    }
    if let Some(signal) = status.signal()
        && let Ok(signal) = ::nix::sys::signal::Signal::try_from(signal)
    {
        // Standalone helper: no runtime or application signal policy is active.
        // If inherited disposition suppresses the signal, return a failure.
        let _ = ::nix::sys::signal::raise(signal);
    }
    1
}

#[cfg(test)]
fn fixture_barrier(stage: &str) -> io::Result<()> {
    let pause = std::env::var("CONFCTL_TEST_ROLLBACK_PAUSE").ok();
    if pause.as_deref() != Some(stage) && pause.as_deref() != Some("both") {
        return Ok(());
    }
    let root = PathBuf::from(
        std::env::var_os("CONFCTL_TEST_ROLLBACK_ROOT")
            .ok_or_else(|| io::Error::other("missing rollback fixture root"))?,
    );
    if !root.is_absolute() {
        return Err(io::Error::other("rollback fixture root must be absolute"));
    }
    let barrier = root.join("barrier");
    let pending = barrier.join(format!("{stage}.pending"));
    std::fs::write(&pending, std::process::id().to_string())?;
    std::fs::rename(pending, barrier.join(format!("{stage}.ready")))?;
    while !barrier.join(format!("{stage}.release")).try_exists()? {
        std::thread::sleep(Duration::from_millis(1));
    }
    Ok(())
}

#[cfg(test)]
fn fixture_wait(kind: &str, child_pid: u32, status: ExitStatus) -> io::Result<()> {
    use std::os::unix::process::ExitStatusExt;
    let Some(root) = std::env::var_os("CONFCTL_TEST_ROLLBACK_ROOT") else {
        return Ok(());
    };
    let root = PathBuf::from(root);
    if !root.is_absolute() {
        return Err(io::Error::other("rollback fixture root must be absolute"));
    }
    // Written only after the actual common wait returns a native status. One
    // file per owner avoids interprocess append races; switch waits are serial.
    let owner = std::process::id();
    let mut record = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(root.join(format!("waits-{owner}.jsonl")))?;
    let bytes = serde_json::to_vec(&serde_json::json!({
        "owner_pid": owner, "child_pid": child_pid, "kind": kind,
        "raw_status": status.into_raw(), "exit_status": status.code(),
        "signal": status.signal()
    }))
    .map_err(io::Error::other)?;
    record.write_all(&bytes)?;
    record.write_all(b"\n")
}

#[cfg(not(test))]
fn invoke(current_system: &Path) -> i32 {
    invoke_inner(current_system)
}

#[cfg(test)]
pub(super) fn invoke(current_system: &Path) -> i32 {
    invoke_inner(current_system)
}

pub fn entry() -> i32 {
    invoke(Path::new("/run/current-system"))
}

fn invoke_inner(current_system: &Path) -> i32 {
    let mut args = std::env::args_os();
    let program = args
        .next()
        .unwrap_or_else(|| "confctl-auto-rollback".into());
    let args = args.collect::<Vec<_>>();
    let is_worker = args.first().is_some_and(|arg| arg == WORKER);
    let result = (|| -> io::Result<i32> {
        if is_worker {
            // This must precede runtime/threads, state reads/writes and switch
            // effects. Do not put this child into a fresh process group first.
            #[cfg(test)]
            fixture_barrier("pre-setsid")?;
            rustix::process::setsid()?;
            #[cfg(test)]
            fixture_barrier("post-setsid")?;
            let Parsed::Run(options) = parse(&args[1..])? else {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "invalid private rollback worker invocation",
                ));
            };
            return worker(options, current_system);
        }
        write_transport(&mut io::stdout(), b"Deploying with auto-rollback\n")?;
        match parse(&args)? {
            Parsed::Help => {
                write_transport(&mut io::stdout(), help(&program).as_bytes())?;
                Ok(0)
            }
            Parsed::Invalid => {
                writeln!(io::stderr(), "Invalid arguments")?;
                io::stderr().write_all(help(&program).as_bytes())?;
                Ok(1)
            }
            Parsed::Run(_) => {
                let mut child = Command::new(std::env::current_exe()?)
                    .arg(WORKER)
                    .args(&args)
                    .stdin(Stdio::null())
                    .spawn()?;
                let status = child.wait()?;
                #[cfg(test)]
                fixture_wait("worker", child.id(), status)?;
                Ok(mirror(status))
            }
        }
    })();
    match result {
        Ok(code) => code,
        Err(error) => {
            let _ = writeln!(io::stderr(), "{error}");
            1
        }
    }
}

#[cfg(test)]
mod tests {
    #[test]
    fn worker_failure_retains_first_source_and_ordered_secondary_without_debug_data() {
        use super::*;
        let error = final_result(
            0,
            vec![
                io::Error::new(io::ErrorKind::PermissionDenied, "private-primary"),
                io::Error::new(io::ErrorKind::UnexpectedEof, "private-secondary"),
            ],
        )
        .unwrap_err();
        let source = error
            .get_ref()
            .unwrap()
            .downcast_ref::<WorkerFailure>()
            .unwrap();
        assert_eq!(source.primary.kind(), io::ErrorKind::PermissionDenied);
        assert_eq!(source.secondary.len(), 1);
        assert_eq!(source.secondary[0].kind(), io::ErrorKind::UnexpectedEof);
        assert!(std::error::Error::source(source).is_some());
        assert!(!format!("{error:?}").contains("private-"));
    }

    #[test]
    fn drainer_keeps_writer_failure_before_later_reader_failure() {
        use super::*;
        struct FailingReader(bool);
        impl Read for FailingReader {
            fn read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
                if self.0 {
                    return Err(io::Error::from(io::ErrorKind::UnexpectedEof));
                }
                self.0 = true;
                bytes[0] = b'x';
                Ok(1)
            }
        }
        struct FailingWriter;
        impl Write for FailingWriter {
            fn write(&mut self, _: &[u8]) -> io::Result<usize> {
                Err(io::Error::from(io::ErrorKind::PermissionDenied))
            }
            fn flush(&mut self) -> io::Result<()> {
                Ok(())
            }
        }
        let error = drain(FailingReader(false), FailingWriter).unwrap_err();
        let source = error
            .get_ref()
            .unwrap()
            .downcast_ref::<WorkerFailure>()
            .unwrap();
        assert_eq!(source.primary.kind(), io::ErrorKind::PermissionDenied);
        assert_eq!(source.secondary[0].kind(), io::ErrorKind::UnexpectedEof);
    }

    #[test]
    fn worker_confirm_and_timeout_preserve_handshake_old_target_action_and_status_quirk() {
        use super::*;
        use std::os::unix::fs::{PermissionsExt, symlink};

        fn system(dir: &Path, name: &str, check: &Path, record: &Path) -> PathBuf {
            let system = dir.join(name);
            std::fs::create_dir_all(system.join("bin")).unwrap();
            let quote =
                |path: &Path| format!("'{}'", path.to_str().unwrap().replace('\'', "'\\''"));
            let program = system.join("bin/switch-to-configuration");
            std::fs::write(&program,format!("#!/bin/sh\nprintf '%s\\n' '{name}' \"$@\" >> {}\ntest \"$(cat {})\" = switching || test '{name}' = old || exit 81\nexit 7\n",quote(record),quote(check))).unwrap();
            std::fs::set_permissions(&program, std::fs::Permissions::from_mode(0o755)).unwrap();
            system
        }
        for confirm in [true, false] {
            let dir = tempfile::tempdir().unwrap();
            let check = dir.path().join("check");
            let record = dir.path().join("switches");
            let old = system(dir.path(), "old", &check, &record);
            let new = system(dir.path(), "new", &check, &record);
            let current = dir.path().join("current-system");
            symlink(&old, &current).unwrap();
            let options = Options {
                timeout: Timeout::parse(if confirm { "60" } else { "0" }).unwrap(),
                toplevel: new,
                action: "boot".into(),
                check_file: check.clone(),
            };
            let stopped = std::sync::atomic::AtomicBool::new(false);
            let result = std::thread::scope(|scope| {
                let confirmer = confirm.then(|| {
                    scope.spawn(|| {
                        while !stopped.load(std::sync::atomic::Ordering::SeqCst) {
                            if std::fs::read(&check).ok().as_deref() == Some(b"switched") {
                                std::fs::write(&check, b"confirmed").unwrap();
                                return;
                            }
                            std::thread::sleep(Duration::from_millis(1));
                        }
                    })
                });
                let result = worker(options, &current);
                stopped.store(true, std::sync::atomic::Ordering::SeqCst);
                if let Some(confirmer) = confirmer {
                    confirmer.join().unwrap();
                }
                result
            });
            assert_eq!(result.unwrap(), if confirm { 0 } else { 1 });
            assert_eq!(
                std::fs::read_to_string(record).unwrap(),
                if confirm {
                    "new\nboot\n"
                } else {
                    "new\nboot\nold\nboot\n"
                }
            );
            if confirm {
                assert!(!check.exists());
            } else {
                assert_eq!(std::fs::read(check).unwrap(), b"switched");
            }
        }
    }

    #[test]
    fn timeout_integer_conversion_and_strict_after_check_match_original_order() {
        use super::*;
        let original: serde_json::Value =
            serde_json::from_str(include_str!("ruby-integers.json")).unwrap();
        for case in original["cases"].as_array().unwrap() {
            let input = case["input"].as_str().unwrap();
            match case.get("timeout") {
                Some(expected) => assert_eq!(
                    Timeout::parse(input).unwrap().display(),
                    expected.as_str().unwrap(),
                    "{input:?}"
                ),
                None => assert!(Timeout::parse(input).is_err(), "{input:?}"),
            }
        }
        let start = SystemTime::UNIX_EPOCH + Duration::from_secs(1_000);
        for (input, decimal) in [
            ("60", "60"),
            ("0x10", "16"),
            ("0b11", "3"),
            ("010", "8"),
            ("1_000", "1000"),
            ("+0", "0"),
            ("-0", "0"),
            ("184467440737095516160001", "184467440737095516160001"),
        ] {
            assert_eq!(Timeout::parse(input).unwrap().display(), decimal);
        }
        assert!(!Timeout::parse("0").unwrap().expired(start, start));
        assert!(
            Timeout::parse("0")
                .unwrap()
                .expired(start, start + Duration::from_nanos(1))
        );
        assert!(Timeout::parse("-1").unwrap().expired(start, start));
        assert!(
            !Timeout::parse("184467440737095516160001")
                .unwrap()
                .expired(start, start + Duration::from_secs(3600))
        );
        for invalid in ["", "x", "08", "1__2", "1_", "_1", "1.2"] {
            assert!(Timeout::parse(invalid).is_err(), "{invalid}");
        }
    }

    #[test]
    fn broken_transport_still_drains_beyond_pipe_capacity_to_eof() {
        use super::*;
        struct BrokenOutput;
        impl Write for BrokenOutput {
            fn write(&mut self, _: &[u8]) -> io::Result<usize> {
                Err(io::ErrorKind::BrokenPipe.into())
            }
            fn flush(&mut self) -> io::Result<()> {
                Err(io::ErrorKind::BrokenPipe.into())
            }
        }
        let bytes = vec![b'x'; 2 * 1024 * 1024];
        let mut input = io::Cursor::new(&bytes);
        drain(&mut input, BrokenOutput).unwrap();
        assert_eq!(input.position(), bytes.len() as u64);
        write_transport(&mut BrokenOutput, b"watchdog continues").unwrap();
    }
}
