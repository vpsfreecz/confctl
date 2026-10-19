//! Closed cfg-test-only common-route records. No product module declaration.
//! The standalone fixture owns start/take; serialization stays here. Helpers only append
//! actual arguments/results at the enumerated sites, never callbacks/executors.

#![cfg(test)]

use std::{
    cell::RefCell,
    ffi::OsString,
    fs::File,
    io::{self, Write},
    os::{
        fd::AsRawFd,
        unix::{ffi::OsStrExt, process::ExitStatusExt},
    },
    path::{Path, PathBuf},
    process::{Command, ExitStatus},
};

#[derive(Debug, PartialEq, Eq)]
pub(super) struct CommandArgs {
    pub(super) program: OsString,
    pub(super) args: Vec<OsString>,
}

impl CommandArgs {
    fn from_command(command: &Command) -> Self {
        Self {
            program: command.get_program().to_owned(),
            args: command.get_args().map(|arg| arg.to_owned()).collect(),
        }
    }
}

#[derive(Debug)]
pub(super) enum Event {
    CommandStart {
        command: CommandArgs,
    },
    CommandWait {
        command: CommandArgs,
        raw_status: i32,
        code: Option<i32>,
        signal: Option<i32>,
    },
    LockOpen {
        path: PathBuf,
    },
    LockAcquired {
        fd: i32,
    },
    PrepareCompleted {
        machines: usize,
    },
    EntropyOpen {
        path: PathBuf,
    },
    EntropyRead {
        bytes: [u8; 3],
    },
    PublishEntered {
        tftp: PathBuf,
        http: PathBuf,
        suffix: String,
    },
    InstallEntered {
        target: PathBuf,
        new: PathBuf,
    },
    CleanupEntered {
        target: PathBuf,
        old: Option<PathBuf>,
    },
    CmdlineRead {
        path: PathBuf,
    },
    IndexGet {
        url: String,
    },
}

#[derive(Default, Debug)]
pub(super) struct Trace {
    pub(super) events: Vec<Event>,
    pub(super) pending_commands: Vec<CommandArgs>,
    pub(super) infrastructure_errors: Vec<&'static str>,
}

thread_local! {
    pub(super) static TRACE: RefCell<Option<Trace>> = const { RefCell::new(None) };
}

pub(super) fn record(event: Event) {
    TRACE.with(|cell| {
        if let Some(trace) = cell.borrow_mut().as_mut() {
            trace.events.push(event);
        }
    });
}

pub(super) fn command_start(command: &Command) {
    TRACE.with(|cell| {
        if let Some(trace) = cell.borrow_mut().as_mut() {
            trace
                .pending_commands
                .push(CommandArgs::from_command(command));
            trace.events.push(Event::CommandStart {
                command: CommandArgs::from_command(command),
            });
        }
    });
}

pub(super) fn command_wait(command: &Command, status: &ExitStatus) {
    TRACE.with(|cell| {
        if let Some(trace) = cell.borrow_mut().as_mut() {
            let arguments = CommandArgs::from_command(command);
            if trace.pending_commands.len() == 1
                && trace.pending_commands.last() == Some(&arguments)
            {
                let _ = trace.pending_commands.pop();
            } else {
                trace
                    .infrastructure_errors
                    .push("returned command status lacks one matching start record");
            }
            trace.events.push(Event::CommandWait {
                command: arguments,
                raw_status: (*status).into_raw(),
                code: status.code(),
                signal: status.signal(),
            });
        }
    });
}

pub(super) fn lock_acquired(file: &File) {
    record(Event::LockAcquired {
        fd: file.as_raw_fd(),
    });
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

pub(super) fn path_value(path: &Path) -> serde_json::Value {
    serde_json::json!({"path_bytes_hex": hex(path.as_os_str().as_bytes())})
}

pub(super) fn command_value(command: &CommandArgs) -> serde_json::Value {
    serde_json::json!({"program_bytes_hex": hex(command.program.as_bytes()),
                      "args_bytes_hex": command.args.iter().map(|arg| hex(arg.as_bytes())).collect::<Vec<_>>()})
}

pub(super) fn event_value(event: &Event) -> serde_json::Value {
    match event {
        Event::CommandStart { command } => {
            serde_json::json!({"site":"command-start", "command": command_value(command)})
        }
        Event::CommandWait {
            command,
            raw_status,
            code,
            signal,
        } => {
            serde_json::json!({"site":"command-wait-returned", "command":command_value(command), "raw_status":raw_status, "exit_code":code, "signal":signal})
        }
        Event::LockOpen { path } => {
            serde_json::json!({"site":"lock-open", "path":path_value(path)})
        }
        Event::LockAcquired { fd } => serde_json::json!({"site":"lock-acquired", "fd":fd}),
        Event::PrepareCompleted { machines } => {
            serde_json::json!({"site":"prepare-completed", "machines":machines})
        }
        Event::EntropyOpen { path } => {
            serde_json::json!({"site":"entropy-open", "path":path_value(path)})
        }
        Event::EntropyRead { bytes } => {
            serde_json::json!({"site":"entropy-read-completed", "bytes":bytes})
        }
        Event::PublishEntered { tftp, http, suffix } => {
            serde_json::json!({"site":"publish-entered", "tftp":path_value(tftp), "http":path_value(http), "suffix":suffix})
        }
        Event::InstallEntered { target, new } => {
            serde_json::json!({"site":"install-entered", "target":path_value(target), "new":path_value(new)})
        }
        Event::CleanupEntered { target, old } => {
            serde_json::json!({"site":"cleanup-entered", "target":path_value(target), "old":old.as_deref().map(path_value)})
        }
        Event::CmdlineRead { path } => {
            serde_json::json!({"site":"cmdline-read", "path":path_value(path)})
        }
        Event::IndexGet { url } => serde_json::json!({"site":"index-get", "logical_url":url}),
    }
}

pub(super) fn write_receipt(path: &Path, value: &serde_json::Value) -> io::Result<()> {
    let bytes = serde_json::to_vec_pretty(value).map_err(io::Error::other)?;
    let mut file = File::create_new(path)?;
    file.write_all(&bytes)?;
    file.write_all(b"\n")?;
    file.sync_all()
}

#[cfg(test)]
mod tests {
    #[test]
    fn receipt_io_errors_return_and_preserve_existing_output() {
        use super::*;

        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("receipt.json");
        let value = serde_json::json!({"helper_finish_code": 1, "pending_commands": []});
        write_receipt(&path, &value).unwrap();
        let bytes = std::fs::read(&path).unwrap();
        assert_eq!(bytes.last(), Some(&b'\n'));
        assert_eq!(
            serde_json::from_slice::<serde_json::Value>(&bytes).unwrap(),
            value
        );
        assert_eq!(
            write_receipt(&path, &value).unwrap_err().kind(),
            io::ErrorKind::AlreadyExists
        );
        assert_eq!(std::fs::read(&path).unwrap(), bytes);
        assert_eq!(
            write_receipt(&dir.path().join("missing/receipt.json"), &value)
                .unwrap_err()
                .kind(),
            io::ErrorKind::NotFound
        );
    }

    #[test]
    fn receipt_preserves_native_path_argv_and_returned_wait_status() {
        use super::*;
        use std::os::unix::ffi::OsStringExt;

        let path = PathBuf::from(OsString::from_vec(b"/owned/log-\xff\n".to_vec()));
        assert_eq!(
            path_value(&path),
            serde_json::json!({"path_bytes_hex":"2f6f776e65642f6c6f672dff0a"})
        );
        let mut command = Command::new("/bin/sh");
        command.args([
            OsString::from("-c"),
            OsString::from("exit 9"),
            OsString::from_vec(b"arg-\xff".to_vec()),
        ]);
        TRACE.with(|cell| *cell.borrow_mut() = Some(Trace::default()));
        command_start(&command);
        let status = command.status().expect("inert exit status");
        command_wait(&command, &status);
        let trace = TRACE.with(|cell| cell.borrow_mut().take().unwrap());
        assert!(trace.pending_commands.is_empty());
        assert!(trace.infrastructure_errors.is_empty());
        assert_eq!(trace.events.len(), 2);
        let arguments = serde_json::json!({
            "program_bytes_hex":"2f62696e2f7368",
            "args_bytes_hex":["2d63","657869742039","6172672dff"]
        });
        assert_eq!(
            event_value(&trace.events[0]),
            serde_json::json!({
                "site":"command-start", "command":arguments
            })
        );
        assert_eq!(
            event_value(&trace.events[1]),
            serde_json::json!({
                "site":"command-wait-returned", "command":arguments,
                "raw_status":status.into_raw(), "exit_code":9, "signal":null
            })
        );
    }
}
