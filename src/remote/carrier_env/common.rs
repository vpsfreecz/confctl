use std::{
    ffi::{OsStr, OsString},
    io::{self, Write},
    path::{Path, PathBuf},
    process::{Command, ExitStatus},
};

struct CarrierEnvConfig {
    on_change: PathBuf,
}

struct CommandFailure {
    program: OsString,
    args: Vec<OsString>,
    status: ExitStatus,
}

impl std::fmt::Debug for CommandFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CommandFailure")
            .field("status", &self.status)
            .finish_non_exhaustive()
    }
}

impl std::fmt::Display for CommandFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.program.to_string_lossy())?;
        for argument in &self.args {
            write!(f, " {}", argument.to_string_lossy())?;
        }
        write!(f, " failed")
    }
}

impl std::error::Error for CommandFailure {}

#[derive(Debug, PartialEq, Eq)]
enum Action {
    Set(OsString),
    Delete(OsString),
}

#[derive(Debug, PartialEq, Eq)]
struct Options {
    profile: OsString,
    action: Action,
}

enum Parsed {
    Run(Options),
    Help,
    Invalid(&'static str, bool),
}

fn parse(args: &[OsString]) -> io::Result<Parsed> {
    let mut profile = None;
    let mut action = None;
    let mut surplus = false;
    let mut arguments = args.iter();
    while let Some(argument) = arguments.next() {
        let text = argument.to_str().unwrap_or("");
        if text == "--" {
            surplus |= arguments.next().is_some();
            break;
        }
        if matches!(text, "-h" | "--help") {
            return Ok(Parsed::Help);
        }
        let (option, attached) = text
            .split_once('=')
            .map_or((text, None), |(a, b)| (a, Some(b)));
        let canonical = if option == "-p" || option.starts_with("-p") && !option.starts_with("--") {
            "--profile"
        } else if option.starts_with("--") {
            let candidates: Vec<_> = ["--profile", "--set", "--delete-generations"]
                .into_iter()
                .filter(|name| name.starts_with(option))
                .collect();
            match candidates.as_slice() {
                [name] => *name,
                [] => {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidInput,
                        format!("invalid option: {}", argument.to_string_lossy()),
                    ));
                }
                _ => {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidInput,
                        format!("ambiguous option: {}", argument.to_string_lossy()),
                    ));
                }
            }
        } else if text.starts_with('-') {
            return Err(io::Error::new(
                io::ErrorKind::InvalidInput,
                format!("invalid option: {}", argument.to_string_lossy()),
            ));
        } else {
            surplus = true;
            continue;
        };
        let value = if option.starts_with("-p") && option != "-p" && !option.starts_with("--") {
            OsString::from(&option[2..])
        } else if let Some(value) = attached {
            value.into()
        } else {
            arguments.next().cloned().ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!("missing argument: {option}"),
                )
            })?
        };
        match canonical {
            "--profile" => profile = Some(value),
            "--set" => action = Some(Action::Set(value)),
            "--delete-generations" => action = Some(Action::Delete(value)),
            _ => unreachable!(),
        }
    }
    if surplus {
        Ok(Parsed::Invalid("Too many arguments", true))
    } else if action.is_none() {
        Ok(Parsed::Invalid("No action specified", false))
    } else if let Some(profile) = profile {
        Ok(Parsed::Run(Options {
            profile,
            action: action.expect("validated action"),
        }))
    } else {
        Ok(Parsed::Invalid("Profile not set", false))
    }
}

fn help(program: &OsStr) -> String {
    format!(
        "Usage: {} <options>\n    -p, --profile PROFILE            Profile path\n        --set GENERATION             Set profile to generation\n        --delete-generations GENERATIONS\n                                     Delete generations\n",
        program.to_string_lossy()
    )
}

fn checked(command: &mut Command) -> io::Result<()> {
    #[cfg(test)]
    super::helper_diagnostic_receipts::command_start(command);
    let status = command.status()?;
    #[cfg(test)]
    super::helper_diagnostic_receipts::command_wait(command, &status);
    if status.success() {
        Ok(())
    } else {
        // HELPER-DIAG-01 preserves the command primary and native result.
        // This display text is never reparsed to execute a command.
        Err(io::Error::other(CommandFailure {
            program: command.get_program().to_owned(),
            args: command.get_args().map(OsStr::to_owned).collect(),
            status,
        }))
    }
}

fn prepare(
    program: &OsStr,
    args: &[OsString],
    stdout: &mut impl Write,
    stderr: &mut impl Write,
) -> io::Result<Result<Options, i32>> {
    let options = match parse(args)? {
        Parsed::Run(options) => options,
        Parsed::Help => {
            stdout.write_all(help(program).as_bytes())?;
            return Ok(Err(0));
        }
        Parsed::Invalid(message, usage) => {
            writeln!(stderr, "{message}")?;
            // CARRIER-01: the Ruby branch referenced undefined `parser`.
            // Preserve its intended help/stdout, failure and zero effects.
            if usage {
                stdout.write_all(help(program).as_bytes())?;
            }
            return Ok(Err(1));
        }
    };
    Ok(Ok(options))
}

fn execute(config: &CarrierEnvConfig, options: Options, nix_env: &Path) -> io::Result<i32> {
    let mut command = Command::new(nix_env);
    command.args([OsStr::new("-p"), &options.profile]);
    match options.action {
        Action::Set(generation) => {
            command.arg("--set").arg(generation);
        }
        Action::Delete(generations) => {
            command.arg("--delete-generations").arg(generations);
        }
    }
    checked(&mut command)?;
    checked(&mut Command::new(&config.on_change))?;
    Ok(0)
}

fn package_config() -> io::Result<CarrierEnvConfig> {
    let on_change = std::env::var_os("CONFCTL_INTERNAL_CARRIER_ON_CHANGE")
        .map(PathBuf::from)
        .filter(|path| path.is_absolute())
        .ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                "missing or invalid package field CONFCTL_INTERNAL_CARRIER_ON_CHANGE",
            )
        })?;
    Ok(CarrierEnvConfig { on_change })
}

fn invoke(
    program: &OsStr,
    args: &[OsString],
    stdout: &mut impl Write,
    stderr: &mut impl Write,
) -> io::Result<i32> {
    let options = match prepare(program, args, stdout, stderr)? {
        Ok(options) => options,
        Err(code) => return Ok(code),
    };
    execute(&package_config()?, options, Path::new("nix-env"))
}

fn common_entry(program: &OsStr, args: &[OsString], stdout: &mut impl Write, stderr: &mut impl Write) -> i32 {
    let result = invoke(program, args, stdout, stderr);
    finish(result, stderr)
}


fn finish(result: io::Result<i32>, stderr: &mut impl Write) -> i32 {
    match result {
        Ok(code) => code,
        Err(error) => {
            let _ = writeln!(stderr, "{error}");
            1
        }
    }
}
