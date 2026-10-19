//! Harness-free test artifact. Never installed. The initial guardian alone is
//! a subreaper; the real helper parent/worker use their unchanged common invoke.
//! No libtest/runtime/thread is started before the worker's setsid.

#[path = "../../../../src/remote/auto_rollback.rs"]
mod auto_rollback;
#[path = "../../../../src/remote/ruby_integer.rs"]
mod ruby_integer;

use std::{
    io,
    os::unix::process::{CommandExt, ExitStatusExt},
    path::PathBuf,
    process::{Command, Stdio},
};

struct GuardianFailure {
    primary: io::Error,
    secondary: Vec<io::Error>,
}

impl std::fmt::Debug for GuardianFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("GuardianFailure")
            .field("primary_kind", &self.primary.kind())
            .field("secondary_count", &self.secondary.len())
            .finish_non_exhaustive()
    }
}

impl std::fmt::Display for GuardianFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.primary)?;
        for error in &self.secondary {
            write!(f, "; secondary: {error}")?;
        }
        Ok(())
    }
}

impl std::error::Error for GuardianFailure {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(&self.primary)
    }
}

fn write_report(root: &std::path::Path, name: &str, value: &serde_json::Value) -> io::Result<()> {
    let pending = root.join(format!("{name}.pending"));
    std::fs::write(
        &pending,
        serde_json::to_vec_pretty(value).map_err(io::Error::other)?,
    )?;
    std::fs::rename(pending, root.join(name))
}

fn root() -> io::Result<PathBuf> {
    let path = std::env::var_os("CONFCTL_TEST_ROLLBACK_ROOT")
        .map(PathBuf::from)
        .filter(|path| path.is_absolute())
        .ok_or_else(|| io::Error::other("missing absolute owned rollback fixture root"))?;
    Ok(path)
}

fn guardian(root: &std::path::Path) -> io::Result<()> {
    // Same safe Linux subreaper operation as the accepted native app fixtures.
    // This guardian is outside the helper parent's process group.
    rustix::process::set_child_subreaper(rustix::process::Pid::from_raw(1))?;
    let mut child = Command::new(std::env::current_exe()?)
        .args(std::env::args_os().skip(1))
        .env_remove("CONFCTL_TEST_ROLLBACK_GUARDIAN")
        // Only the fixture's helper PARENT is a group leader. Its real worker
        // inherits this group; product reexec never sets process_group(0).
        .process_group(0)
        .stdin(Stdio::null())
        .spawn()?;
    // Once spawn succeeds, report failure must not release child ownership.
    let parent_pid = child.id();
    let mut failures = Vec::new();
    if let Err(error) = write_report(
        root,
        "guardian-parent.json",
        &serde_json::json!({"pid":parent_pid}),
    ) {
        failures.push(error);
    }
    let mut direct_wait = "std::process::Child::wait";
    let parent = match child.wait() {
        Ok(status) => Some(status),
        Err(error) => {
            failures.push(error);
            direct_wait = "rustix::process::waitpid fallback after retained wait error";
            // Do not scan adopted children until the exact direct child has
            // been waited or is proven no longer owned. This fallback never
            // competes with a live parent's wait for its own worker.
            loop {
                match rustix::process::waitpid(
                    rustix::process::Pid::from_raw(parent_pid as i32),
                    rustix::process::WaitOptions::empty(),
                ) {
                    Ok(Some((_, status))) => {
                        break Some(std::process::ExitStatus::from_raw(status.as_raw()));
                    }
                    Err(rustix::io::Errno::CHILD) => {
                        failures.push(rustix::io::Errno::CHILD.into());
                        break None;
                    }
                    Err(rustix::io::Errno::INTR) => continue,
                    Ok(None) => failures.push(io::Error::other(
                        "blocking exact fixture wait returned no child",
                    )),
                    Err(error) => failures.push(error.into()),
                }
                // Keep the owner alive on unexpected wait infrastructure
                // errors; native fault qualification must prove termination.
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
        }
    };
    let mut adopted = Vec::new();
    loop {
        match rustix::process::wait(rustix::process::WaitOptions::empty()) {
            Ok(Some((pid, status))) => adopted.push(serde_json::json!({
                "pid":pid.as_raw_nonzero().get(),
                "raw_status":status.as_raw(),
                "exit_status":status.exit_status(),
                "signal":status.terminating_signal()
            })),
            Err(rustix::io::Errno::CHILD) => break,
            Err(rustix::io::Errno::INTR) => continue,
            Ok(None) => failures.push(io::Error::other("blocking fixture wait returned no child")),
            Err(error) => {
                failures.push(error.into());
                std::thread::sleep(std::time::Duration::from_millis(1));
            }
        }
    }
    let parent = parent.map(|status| {
        serde_json::json!({
            "pid":parent_pid, "raw_status":status.into_raw(),
            "exit_status":status.code(), "signal":status.signal(), "wait_api":direct_wait
        })
    });
    let result = serde_json::json!({
        "parent":parent, "adopted":adopted, "no_children":true,
        "retained_error_kinds":failures.iter().map(|error| format!("{:?}",error.kind())).collect::<Vec<_>>()
    });
    if let Err(error) = write_report(root, "guardian-result.json", &result) {
        failures.push(error);
    }
    if failures.is_empty() {
        Ok(())
    } else {
        let mut failures = failures.into_iter();
        let primary = failures.next().expect("nonempty fixture failures");
        Err(io::Error::new(
            primary.kind(),
            GuardianFailure {
                primary,
                secondary: failures.collect(),
            },
        ))
    }
}

fn main() {
    // Keep the production entry referenced in this include-based artifact;
    // the fixture invokes only the accepted private typed path entry below.
    let _product_entry: fn() -> i32 = auto_rollback::entry;
    let result = (|| -> io::Result<i32> {
        let root = root()?;
        if std::env::var_os("CONFCTL_TEST_ROLLBACK_GUARDIAN").is_some() {
            guardian(&root)?;
            return Ok(0);
        }
        // Path construction only: no current-system read or deployment effect
        // occurs until common invoke has successfully detached its worker.
        Ok(auto_rollback::invoke(&root.join("current-system")))
    })();
    match result {
        Ok(code) => std::process::exit(code),
        Err(error) => {
            eprintln!("{error}");
            std::process::exit(1);
        }
    }
}
