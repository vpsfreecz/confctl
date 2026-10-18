//! Compatibility adapter around clap's static option builder. GLI's observable
//! prefix, option-ordering, help/version and error policies remain explicit.

mod requests;
mod schema;

use crate::domain::{ColorMode, GlobalOptions, Invocation};
use clap::{Arg, ArgAction, ArgMatches, Command, builder::OsStringValueParser};
use schema::{Kind, OptionSpec};
use std::{
    collections::BTreeMap,
    ffi::{OsStr, OsString},
};

#[derive(Debug, Eq, PartialEq)]
pub struct CliDisplay {
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub code: i32,
}
#[derive(Debug, Eq, PartialEq)]
pub struct CliFailure {
    pub display: CliDisplay,
    pub message: String,
    pub path: String,
    pub requires_log: bool,
}
#[derive(Debug, Eq, PartialEq)]
pub enum ParseOutcome {
    Run(Invocation),
    Display(CliDisplay),
}

/// Arguments exclude argv[0]. Native child arguments and paths retain OsString.
pub fn parse(argv: Vec<OsString>) -> Result<ParseOutcome, CliFailure> {
    let mut index = 0;
    let mut global = GlobalOptions::default();
    while let Some(token) = argv.get(index).and_then(|s| s.to_str()) {
        if token == "--version" {
            return Ok(ParseOutcome::Display(CliDisplay {
                stdout: vec![],
                stderr: b"confctl: version unknown\nerror: confctl: version unknown\n".to_vec(),
                code: 1,
            }));
        }
        if token == "--help" || token == "-h" {
            return Ok(ParseOutcome::Display(display_help("")));
        }
        if token == "--" {
            index += 1;
            break;
        }
        if token == "--color"
            || token == "-c"
            || token.starts_with("--color=")
            || token.starts_with("-c") && token.len() > 2
        {
            let inline = token
                .split_once('=')
                .map(|(_, v)| v)
                .or_else(|| token.strip_prefix("-c").filter(|s| !s.is_empty()));
            let value = if let Some(value) = inline {
                value.to_owned()
            } else {
                index += 1;
                argv.get(index)
                    .and_then(|s| s.to_str())
                    .ok_or_else(|| failure("", "missing argument: --color", false))?
                    .to_owned()
            };
            global.color = match value.as_str() {
                "always" => ColorMode::Always,
                "never" => ColorMode::Never,
                "auto" => ColorMode::Auto,
                _ => {
                    return Err(failure(
                        "",
                        &format!("invalid argument: --color {value}"),
                        false,
                    ));
                }
            };
            index += 1;
        } else {
            break;
        }
    }
    if index == argv.len() {
        return Ok(ParseOutcome::Display(display_help("")));
    }
    if argv[index] == "help" {
        index += 1;
        if argv.get(index).is_some_and(|s| s == "-c") {
            return Ok(ParseOutcome::Display(CliDisplay {
                stdout: help_text("#completion").into_bytes(),
                stderr: vec![],
                code: 0,
            }));
        }
        let path = resolve_help(&argv[index..])?;
        return Ok(ParseOutcome::Display(display_help(&path)));
    }
    let mut path = String::new();
    loop {
        let Some(token) = argv.get(index).and_then(|s| s.to_str()) else {
            return Err(failure(&path, "command is not valid UTF-8", false));
        };
        if token == "--help" || token == "-h" {
            return Ok(ParseOutcome::Display(display_help(&path)));
        }
        let name = resolve(&path, token)?;
        if !path.is_empty() {
            path.push(' ');
        }
        path.push_str(name);
        index += 1;
        if schema::ROUTES.contains(&path.as_str()) {
            break;
        }
        if index == argv.len() {
            return Ok(ParseOutcome::Display(display_help(&path)));
        }
    }
    let specs = schema::options(&path);
    let (canonical, positional, help) = lex_options(&argv[index..], &specs, &path)?;
    if help {
        return Ok(ParseOutcome::Display(display_help(&path)));
    }
    let mut command = Command::new("confctl")
        .disable_help_flag(true)
        .disable_version_flag(true)
        .args_override_self(true);
    for spec in &specs {
        let action = if matches!(spec.kind, Kind::Repeat) {
            ArgAction::Append
        } else {
            ArgAction::Set
        };
        let mut arg = Arg::new(spec.name)
            .long(spec.name)
            .action(action)
            .allow_hyphen_values(true)
            .value_parser(OsStringValueParser::new());
        if let Kind::Switch(value) = spec.kind {
            arg = arg.default_value(if value { "true" } else { "false" });
        }
        command = command.arg(arg);
    }
    let matches = command
        .try_get_matches_from(std::iter::once(OsString::from("confctl")).chain(canonical))
        .map_err(|_| failure(&path, "invalid command options", false))?;
    if let Some((required, optional)) = schema::arity(&path) {
        if positional.len() < required.len() {
            return Err(failure(
                &path,
                &format!("missing argument <{}>", required[positional.len()]),
                true,
            ));
        }
        if positional.len() > required.len() + optional {
            let extra = &positional[required.len() + optional..];
            let mut message = format!(
                "unknown argument{}: {}",
                if extra.len() > 1 { "s" } else { "" },
                extra
                    .iter()
                    .map(|s| s.to_string_lossy())
                    .collect::<Vec<_>>()
                    .join(" ")
            );
            if extra.iter().any(|s| s.as_encoded_bytes().starts_with(b"-")) {
                message.push_str(" (note that options must come before arguments)");
            }
            return Err(failure(&path, &message, true));
        }
    }
    let values = Values {
        matches: &matches,
        positional: &positional,
        path: &path,
    };
    let action = requests::build(&values)?;
    Ok(ParseOutcome::Run(Invocation { global, action }))
}

fn resolve_help(args: &[OsString]) -> Result<String, CliFailure> {
    let mut path = String::new();
    for arg in args {
        let name = resolve(
            &path,
            arg.to_str()
                .ok_or_else(|| failure(&path, "command is not valid UTF-8", false))?,
        )?;
        if !path.is_empty() {
            path.push(' ');
        }
        path.push_str(name);
    }
    Ok(path)
}
fn resolve<'a>(path: &str, token: &str) -> Result<&'a str, CliFailure> {
    let choices = schema::children(path);
    if let Some(name) = choices.iter().find(|name| **name == token) {
        return Ok(name);
    }
    let names: Vec<_> = choices
        .into_iter()
        .filter(|name| name.starts_with(token))
        .collect();
    match names.as_slice() {
        [name] => Ok(name),
        [] => Err(failure(path, &format!("Unknown command '{token}'"), false)),
        _ => Err(failure(
            path,
            &format!(
                "Ambiguous command '{token}'. It matches {}",
                names.join(",")
            ),
            false,
        )),
    }
}

fn lex_options(
    args: &[OsString],
    specs: &[OptionSpec],
    path: &str,
) -> Result<(Vec<OsString>, Vec<OsString>, bool), CliFailure> {
    let mut canonical = Vec::new();
    let mut index = 0;
    while index < args.len() {
        let token = &args[index];
        if token == "--" {
            return Ok((canonical, args[index + 1..].to_vec(), false));
        }
        let Some(text) = token.to_str() else {
            return Ok((canonical, args[index..].to_vec(), false));
        };
        if !text.starts_with('-') || text == "-" {
            return Ok((canonical, args[index..].to_vec(), false));
        }
        if text == "--help" || text == "-h" {
            return Ok((canonical, vec![], true));
        }
        let long = text.starts_with("--");
        let option = text.trim_start_matches('-');
        let (name, inline) = option
            .split_once('=')
            .map_or((option, None), |(k, v)| (k, Some(OsString::from(v))));
        let (name, negate) = name
            .strip_prefix("no-")
            .map_or((name, false), |name| (name, true));
        let mut candidates: Vec<_> = specs
            .iter()
            .filter(|s| s.name == name || s.aliases.contains(&name))
            .collect();
        let mut attached = inline;
        if candidates.is_empty() && !long && name.len() > 1 {
            let short = &name[..1];
            candidates = specs
                .iter()
                .filter(|s| s.aliases.contains(&short))
                .collect();
            if candidates.len() == 1 && !matches!(candidates[0].kind, Kind::Switch(_)) {
                attached = Some(OsString::from(&name[1..]));
            } else {
                candidates.clear();
            }
        }
        if candidates.is_empty() && long {
            candidates = specs.iter().filter(|s| s.name.starts_with(name)).collect();
        }
        if candidates.len() != 1 {
            return Err(failure(
                path,
                &format!("Unknown option {}", text.split('=').next().unwrap_or(text)),
                false,
            ));
        }
        let spec = candidates[0];
        let value = match spec.kind {
            Kind::Switch(_) => {
                if negate && !spec.negatable || attached.is_some() {
                    return Err(failure(path, &format!("Unknown option {text}"), false));
                }
                OsString::from(if negate { "false" } else { "true" })
            }
            _ => {
                if negate {
                    return Err(failure(path, &format!("Unknown option {text}"), false));
                }
                let value = if let Some(value) = attached {
                    value
                } else {
                    index += 1;
                    args.get(index)
                        .cloned()
                        .ok_or_else(|| failure(path, &format!("missing argument: {text}"), false))?
                };
                if matches!(spec.kind, Kind::Integer)
                    && value.to_str().and_then(|s| s.parse::<i64>().ok()).is_none()
                {
                    return Err(failure(
                        path,
                        &format!("invalid argument: {text} {}", value.to_string_lossy()),
                        false,
                    ));
                }
                if spec.name == "managed"
                    && !matches!(value.to_str(), Some("y" | "yes" | "n" | "no" | "a" | "all"))
                {
                    return Err(failure(
                        path,
                        &format!("invalid argument: {text} {}", value.to_string_lossy()),
                        false,
                    ));
                }
                value
            }
        };
        canonical.push(OsString::from(format!("--{}", spec.name)));
        canonical.push(value);
        index += 1;
    }
    Ok((canonical, vec![], false))
}

pub(super) struct Values<'a> {
    matches: &'a ArgMatches,
    positional: &'a [OsString],
    path: &'a str,
}
impl Values<'_> {
    fn value(&self, name: &str) -> Option<&OsStr> {
        self.matches
            .try_get_one::<OsString>(name)
            .ok()
            .flatten()
            .map(OsString::as_os_str)
    }
    fn string(&self, name: &str) -> Result<Option<String>, CliFailure> {
        self.value(name)
            .map(|v| {
                v.to_str()
                    .map(str::to_owned)
                    .ok_or_else(|| failure(self.path, &format!("{name} is not valid UTF-8"), true))
            })
            .transpose()
    }
    fn flag(&self, name: &str) -> bool {
        self.value(name).is_some_and(|v| v == "true")
    }
    fn integer(&self, name: &str, default: i64) -> i64 {
        self.value(name)
            .and_then(OsStr::to_str)
            .and_then(|s| s.parse().ok())
            .unwrap_or(default)
    }
    fn arg(&self, index: usize) -> Result<Option<String>, CliFailure> {
        self.positional
            .get(index)
            .map(|v| {
                v.to_str()
                    .map(str::to_owned)
                    .ok_or_else(|| failure(self.path, "argument is not valid UTF-8", true))
            })
            .transpose()
    }
    fn all(&self, name: &str) -> Result<Vec<String>, CliFailure> {
        self.matches
            .try_get_many::<OsString>(name)
            .ok()
            .flatten()
            .into_iter()
            .flatten()
            .map(|v| {
                v.to_str()
                    .map(str::to_owned)
                    .ok_or_else(|| failure(self.path, &format!("{name} is not valid UTF-8"), true))
            })
            .collect()
    }
}

fn help_text(path: &str) -> String {
    // Static text was copied from cc40679's immutable help captures. It is
    // product presentation data, independent of the removed Ruby/test files.
    let help: BTreeMap<String, String> =
        serde_json::from_str(include_str!("help.json")).expect("static help schema");
    if let Some(text) = help.get(path) {
        return text.clone();
    }
    format!(
        "NAME\n    {path}\n\nCOMMANDS\n{}",
        schema::children(path)
            .iter()
            .map(|s| format!("    {s}\n"))
            .collect::<String>()
    )
}
fn display_help(path: &str) -> CliDisplay {
    CliDisplay {
        stdout: help_text(path).into_bytes(),
        stderr: vec![],
        code: 0,
    }
}
fn failure(path: &str, message: &str, requires_log: bool) -> CliFailure {
    CliFailure {
        display: CliDisplay {
            stdout: help_text(path).into_bytes(),
            stderr: format!("error: {message}\n\n").into_bytes(),
            code: 64,
        },
        message: message.into(),
        path: path.into(),
        requires_log,
    }
}

#[cfg(test)]
mod tests;
