//! Opt-in, noninstalled COMMON-ROUTE fixture for external qualification drivers.
//! Helpers keep their ordinary command/status paths. Fixture infrastructure
//! errors return nonzero; this target does not qualify installed helper frames.

#[cfg(not(test))]
compile_error!("common-route fixture requires test cfg and is never a product target");

#[path = "diagnostic_fixture/builder.rs"]
mod builder;
#[path = "diagnostic_fixture/carrier.rs"]
mod carrier;
#[path = "../../../../src/remote/helper_diagnostic_receipts.rs"]
mod helper_diagnostic_receipts;
#[path = "diagnostic_fixture/json.rs"]
mod json;
#[path = "diagnostic_fixture/kexec.rs"]
mod kexec;
#[path = "../../../../src/remote/ruby_integer.rs"]
mod ruby_integer;

use confctl::error;

use helper_diagnostic_receipts::{Trace, command_value, event_value, path_value, write_receipt};
use std::{ffi::OsStr, io, net::SocketAddrV4, path::PathBuf};

#[derive(Clone, Copy)]
enum Mode {
    CarrierNixFailure,
    CarrierCallbackFailure,
    NetbootInvalid0,
    NetbootInvalid1,
    NetbootPublishFailure,
    KexecJsonFailure,
}

impl Mode {
    fn parse(value: &OsStr) -> io::Result<Self> {
        match value.to_str() {
            Some("carrier-nix-failure") => Ok(Self::CarrierNixFailure),
            Some("carrier-callback-failure") => Ok(Self::CarrierCallbackFailure),
            Some("netboot-invalid-0") => Ok(Self::NetbootInvalid0),
            Some("netboot-invalid-1") => Ok(Self::NetbootInvalid1),
            Some("netboot-publish-failure") => Ok(Self::NetbootPublishFailure),
            Some("kexec-json-failure") => Ok(Self::KexecJsonFailure),
            _ => Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                "unknown closed common-route mode",
            )),
        }
    }

    fn name(self) -> &'static str {
        match self {
            Self::CarrierNixFailure => "carrier-nix-failure",
            Self::CarrierCallbackFailure => "carrier-callback-failure",
            Self::NetbootInvalid0 => "netboot-invalid-0",
            Self::NetbootInvalid1 => "netboot-invalid-1",
            Self::NetbootPublishFailure => "netboot-publish-failure",
            Self::KexecJsonFailure => "kexec-json-failure",
        }
    }
}

struct Inputs {
    mode: Mode,
    root: PathBuf,
    destination: Option<SocketAddrV4>,
}

fn inputs() -> io::Result<Inputs> {
    let args = std::env::args_os().skip(1).collect::<Vec<_>>();
    let Some(mode) = args.first() else {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "missing common-route mode",
        ));
    };
    let mode = Mode::parse(mode)?;
    let count = if matches!(mode, Mode::KexecJsonFailure) {
        3
    } else {
        2
    };
    if args.len() != count {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "wrong or surplus common-route fixture inputs",
        ));
    }
    let root = PathBuf::from(&args[1]);
    if !root.is_absolute() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "owned absolute common-route root required",
        ));
    }
    let destination = if matches!(mode, Mode::KexecJsonFailure) {
        let address = args[2]
            .to_str()
            .and_then(|value| value.parse::<SocketAddrV4>().ok())
            .filter(|address| address.ip().is_loopback() && address.port() != 0)
            .ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "owned IPv4 loopback socket with nonzero port required",
                )
            })?;
        Some(address)
    } else {
        None
    };
    Ok(Inputs {
        mode,
        root,
        destination,
    })
}

fn invoke(inputs: &Inputs) -> i32 {
    let mut stdout = io::stdout();
    let mut stderr = io::stderr();
    match inputs.mode {
        Mode::CarrierNixFailure | Mode::CarrierCallbackFailure => carrier::fixture_entry(
            &inputs.root,
            matches!(inputs.mode, Mode::CarrierCallbackFailure),
            &mut stdout,
            &mut stderr,
        ),
        Mode::NetbootInvalid0 | Mode::NetbootInvalid1 | Mode::NetbootPublishFailure => {
            builder::invoke(&inputs.root, &mut stdout, &mut stderr)
        }
        Mode::KexecJsonFailure => {
            let destination = inputs
                .destination
                .expect("validated sixth-mode destination");
            kexec::invoke(&inputs.root, destination, &mut stdout, &mut stderr)
        }
    }
}

fn receipt(inputs: &Inputs, helper_code: i32, trace: &Trace) -> serde_json::Value {
    serde_json::json!({
        "scope":"COMMON-ROUTE fixture only; installed native frames remain0/6; not syscall-complete",
        "mode": inputs.mode.name(), "root":path_value(&inputs.root),
        "helper_finish_code":helper_code,
        "events":trace.events.iter().enumerate().map(|(ordinal,event)| serde_json::json!({"ordinal":ordinal,"event":event_value(event)})).collect::<Vec<_>>(),
        "pending_commands":trace.pending_commands.iter().map(command_value).collect::<Vec<_>>(),
        "fixture_infrastructure_errors":trace.infrastructure_errors,
        "wait_proof_limit":"Returned real std::process::ExitStatus after existing status() wait, not a syscall trace or stub identity",
        "ownership":"Missing/unmatched command status or receipt fails qualification; no nested child identity or custody claim",
    })
}

fn activate_trace() -> io::Result<()> {
    helper_diagnostic_receipts::TRACE
        .try_with(|cell| {
            let mut slot = cell.try_borrow_mut().map_err(io::Error::other)?;
            if slot.is_some() {
                return Err(io::Error::other("common-route trace already active"));
            }
            *slot = Some(Trace::default());
            Ok(())
        })
        .map_err(io::Error::other)?
}

fn drain_trace() -> io::Result<Trace> {
    helper_diagnostic_receipts::TRACE
        .try_with(|cell| {
            cell.try_borrow_mut()
                .map_err(io::Error::other)?
                .take()
                .ok_or_else(|| io::Error::other("common-route trace missing"))
        })
        .map_err(io::Error::other)?
}

fn run_fixture(inputs: &Inputs) -> io::Result<i32> {
    activate_trace()?;
    let helper_code = invoke(inputs);
    let trace = drain_trace()?;
    // A command start without a returned status is not successful completion.
    if !trace.pending_commands.is_empty() || !trace.infrastructure_errors.is_empty() {
        return Err(io::Error::other("incomplete common-route command trace"));
    }
    write_receipt(
        &inputs.root.join("audit/common-route.json"),
        &receipt(inputs, helper_code, &trace),
    )?;
    Ok(helper_code)
}

fn main() -> io::Result<std::process::ExitCode> {
    let inputs = inputs()?;
    let code = run_fixture(&inputs)?;
    Ok(std::process::ExitCode::from(code as u8))
}
