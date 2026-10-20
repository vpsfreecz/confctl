//! Borrowed, read-only host observations for generation listing and deployment.

use crate::{
    domain::{Inventory, Machine},
    error::ConfctlError,
    events::TaskId,
    process::{
        Captured, PartialProcessEvidence, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec,
        StdinPolicy, TerminationCause,
    },
};
use chrono::{DateTime, Local, Utc};
use std::{
    ffi::OsStr,
    io::{self, Read},
    path::PathBuf,
};
use tokio_util::sync::CancellationToken;

#[derive(Debug)]
pub(super) struct HostGeneration {
    pub id: String,
    pub toplevel: PathBuf,
    pub date: DateTime<Local>,
    pub kernel: Option<String>,
    pub current: bool,
}

#[derive(Debug, Default)]
pub(super) struct HostStatus {
    pub uptime: Option<f64>,
    pub current_toplevel: Option<PathBuf>,
    pub generations: Option<Vec<HostGeneration>>,
    pub diagnostics: Vec<String>,
    pub failure: Option<QueryFailure>,
}

#[derive(Debug)]
pub(super) struct QueryFailure {
    pub phase: &'static str,
    pub outcome: ProcessOutcome,
    pub stderr: String,
}
impl QueryFailure {
    pub fn diagnostic(&self, host: &str) -> String {
        let status = self
            .outcome
            .code()
            .map(|code| format!("exit status {code}"))
            .unwrap_or_else(|| format!("signal {}", self.outcome.signal().unwrap_or(0)));
        format!(
            "Unable to read {} for {host}: {status}\n{}",
            self.phase, self.stderr
        )
    }
}

pub(super) fn routing<'a>(
    inventory: &'a Inventory,
    machine: &'a Machine,
) -> Result<(&'a Machine, String), ConfctlError> {
    let Some(carrier) = machine.carrier.as_deref() else {
        return Ok((machine, "/nix/var/nix/profiles/system".into()));
    };
    let target = inventory
        .machines
        .iter()
        .find(|candidate| candidate.name == carrier)
        .ok_or_else(|| {
            ConfctlError::Configuration(format!("Carrier {carrier} not found in machine list"))
        })?;
    let alias = machine
        .extra
        .get("alias")
        .filter(|v| !v.is_null() && **v != false)
        .map(|v| {
            v.as_str().ok_or_else(|| {
                ConfctlError::Configuration("carried machine alias is not a string".into())
            })
        })
        .transpose()?
        .unwrap_or(&machine.cluster_name);
    Ok((
        target,
        format!("/nix/var/nix/profiles/confctl-{}", alias.replace('/', ":")),
    ))
}

pub(super) async fn query<P: ProcessRunner>(
    inventory: &Inventory,
    machine: &Machine,
    runner: &P,
    cancellation: &CancellationToken,
    task: TaskId,
    ssh_config: Option<&OsStr>,
    read_current: bool,
) -> Result<HostStatus, ConfctlError> {
    let (target, profile) = routing(inventory, machine)?;
    let command = |argv: &[&str]| {
        let argv = argv.iter().map(|s| (*s).into()).collect::<Vec<_>>();
        let mut spec = super::connection::command_spec(target, ssh_config, &argv)?;
        spec.redacted_description = format!("query generations on {}", machine.name);
        Ok::<_, ConfctlError>(spec)
    };
    let mut status =
        uptime_only(inventory, machine, runner, cancellation, task, ssh_config).await?;
    if status.failure.is_some() {
        return Ok(status);
    }
    if read_current {
        let path = if machine.carrier.is_some() {
            profile.as_str()
        } else {
            "/run/current-system"
        };
        match observe(
            runner,
            command(&["realpath", "-e", "--", path])?,
            cancellation,
            task,
            "current toplevel",
            |text| {
                let path = text.trim_matches(['\0', ' ', '\t', '\r', '\n', '\x0b', '\x0c']);
                if path.is_empty() {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidData,
                        "current toplevel response has no path",
                    ));
                }
                Ok(PathBuf::from(path))
            },
        )
        .await?
        {
            Ok(path) => status.current_toplevel = Some(path),
            Err(failure) => {
                status.failure = Some(failure);
                return Ok(status);
            }
        }
    }
    let mut spec = command(&["bash", "--norc"])?;
    spec.stdin = StdinPolicy::Bytes(profile_script(&profile));
    match observe(
        runner,
        spec,
        cancellation,
        task,
        "profile generations",
        |text| decode_profile(&profile, text),
    )
    .await?
    {
        Ok((generations, diagnostics)) => {
            status.generations = Some(generations);
            status.diagnostics = diagnostics;
        }
        Err(failure) => status.failure = Some(failure),
    }
    if cancellation.is_cancelled() {
        return Err(ConfctlError::Cancelled);
    }
    Ok(status)
}

pub(super) async fn uptime_only<P: ProcessRunner>(
    inventory: &Inventory,
    machine: &Machine,
    runner: &P,
    cancellation: &CancellationToken,
    task: TaskId,
    ssh_config: Option<&OsStr>,
) -> Result<HostStatus, ConfctlError> {
    let (target, _) = routing(inventory, machine)?;
    let mut spec = super::connection::command_spec(
        target,
        ssh_config,
        &["cat".into(), "/proc/uptime".into()],
    )?;
    spec.redacted_description = format!("query generations on {}", machine.name);
    uptime_spec(runner, cancellation, task, spec).await
}

pub(super) async fn uptime_spec<P: ProcessRunner>(
    runner: &P,
    cancellation: &CancellationToken,
    task: TaskId,
    spec: ProcessSpec,
) -> Result<HostStatus, ConfctlError> {
    let mut status = HostStatus::default();
    match observe(runner, spec, cancellation, task, "uptime", |text| {
        text.split_whitespace()
            .next()
            .unwrap_or("")
            .parse::<f64>()
            .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))
    })
    .await?
    {
        Ok(uptime) => status.uptime = Some(uptime),
        Err(failure) => {
            status.failure = Some(failure);
            return Ok(status);
        }
    }
    if cancellation.is_cancelled() {
        return Err(ConfctlError::Cancelled);
    }
    Ok(status)
}

fn profile_script(profile: &str) -> Vec<u8> {
    use std::os::unix::ffi::OsStrExt;
    let quoted = super::connection::shell_quote(OsStr::new(profile));
    let mut bytes = b"profile=".to_vec();
    bytes.extend_from_slice(quoted.as_bytes());
    bytes.extend_from_slice(
        br#"
realpath -e -- "$profile" || exit $?
for generation in "$profile"-*-link; do
  [ -e "$generation" ] || [ -L "$generation" ] || continue
  target=$(readlink "$generation") || exit $?
  created_at=$(stat --format=%Y "$generation") || exit $?
  printf '%s;%s;%s;' "$generation" "$target" "$created_at"
  for kernel_file in kernel bzImage; do
    if [ -h "$generation/$kernel_file" ]; then
      kernel=$(readlink "$generation/$kernel_file") || exit $?
      printf '%s' "$kernel"
    fi
  done
  printf '\n'
done
"#,
    );
    bytes
}

fn decode_profile(profile: &str, text: &str) -> io::Result<(Vec<HostGeneration>, Vec<String>)> {
    let mut lines = text
        .trim_matches(['\0', ' ', '\t', '\r', '\n', '\x0b', '\x0c'])
        .lines();
    let current = lines.next().filter(|s| !s.is_empty()).ok_or_else(|| {
        io::Error::new(
            io::ErrorKind::InvalidData,
            "profile response has no current-path header",
        )
    })?;
    let mut generations = Vec::new();
    let mut diagnostics = Vec::new();
    for line in lines {
        let mut fields = line.split(';');
        let link = fields.next().unwrap_or("");
        let id = link
            .strip_prefix(profile)
            .and_then(|s| s.strip_prefix('-'))
            .and_then(|s| s.strip_suffix("-link"))
            .filter(|s| !s.is_empty() && s.bytes().all(|b| b.is_ascii_digit()));
        let Some(id) = id else {
            diagnostics.push(format!("Invalid profile generation link '{link}'"));
            continue;
        };
        let toplevel = fields.next().unwrap_or("");
        let created = fields.next().unwrap_or("");
        let created = timestamp(created)?;
        let date = DateTime::<Utc>::from_timestamp(created, 0)
            .ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::InvalidData,
                    "profile generation timestamp is out of range",
                )
            })?
            .with_timezone(&Local);
        let id = id.trim_start_matches('0');
        generations.push(HostGeneration {
            id: if id.is_empty() { "0".into() } else { id.into() },
            toplevel: toplevel.into(),
            date,
            kernel: fields.next().and_then(kernel_version),
            current: toplevel == current,
        });
    }
    generations.sort_by(|a, b| (a.id.len(), &a.id).cmp(&(b.id.len(), &b.id)));
    Ok((generations, diagnostics))
}

fn timestamp(text: &str) -> io::Result<i64> {
    let text = text.trim_start_matches([' ', '\t', '\n', '\x0b', '\x0c', '\r']);
    let digits = text.strip_prefix(['-', '+']).unwrap_or(text);
    let len = digits.bytes().take_while(u8::is_ascii_digit).count();
    if len == 0 {
        return Ok(0);
    }
    text[..text.len() - digits.len() + len]
        .parse()
        .map_err(|error| io::Error::new(io::ErrorKind::InvalidData, error))
}

pub(super) fn kernel_version(link: &str) -> Option<String> {
    let (hash, rest) = link.strip_prefix("/nix/store/")?.split_once('-')?;
    if hash.is_empty() || hash.contains('/') {
        return None;
    }
    rest.strip_prefix("linux-")?
        .split('/')
        .next()
        .filter(|s| !s.is_empty())
        .map(str::to_owned)
}

fn captured(capture: &Captured) -> io::Result<Vec<u8>> {
    match capture {
        Captured::Memory(bytes) => Ok(bytes.clone()),
        Captured::Spool { file, length } => {
            let mut bytes = Vec::new();
            file.reopen()?.take(*length).read_to_end(&mut bytes)?;
            if bytes.len() as u64 != *length {
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "incomplete host observation spool",
                ));
            }
            Ok(bytes)
        }
        Captured::Unavailable => Err(io::Error::other("host observation stream was not captured")),
    }
}

async fn observe<P: ProcessRunner, T>(
    runner: &P,
    spec: ProcessSpec,
    cancellation: &CancellationToken,
    task: TaskId,
    phase: &'static str,
    decode: impl FnOnce(&str) -> io::Result<T>,
) -> Result<Result<T, QueryFailure>, ConfctlError> {
    if cancellation.is_cancelled() {
        return Err(ConfctlError::Cancelled);
    }
    let outcome = runner
        .run(spec, ProcessScope::new(cancellation.clone(), task))
        .await?;
    let result = (|| {
        if outcome.termination == TerminationCause::Canceled || cancellation.is_cancelled() {
            return Err(ConfctlError::Cancelled);
        }
        let decode_output = || -> io::Result<Result<T, String>> {
            if !outcome.stdout_complete || !outcome.stderr_complete {
                return Err(io::Error::new(
                    io::ErrorKind::UnexpectedEof,
                    "incomplete host observation capture",
                ));
            }
            let stdout = captured(&outcome.stdout)?;
            let stderr = captured(&outcome.stderr)?;
            if !outcome.status.success() {
                return Ok(Err(String::from_utf8_lossy(&stderr).into_owned()));
            }
            let text = std::str::from_utf8(&stdout)
                .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?;
            decode(text).map(Ok)
        };
        decode_output().map_err(|source| ConfctlError::Io {
            operation: phase,
            path: None,
            source,
        })
    })();
    match result {
        Ok(Ok(value)) => Ok(Ok(value)),
        Ok(Err(stderr)) => Ok(Err(QueryFailure {
            phase,
            outcome,
            stderr,
        })),
        Err(source) => Err(ConfctlError::ProcessFailure {
            source: Box::new(source),
            evidence: Box::new(PartialProcessEvidence {
                status: Some(outcome.status),
                stdout: outcome.stdout,
                stderr: outcome.stderr,
                stdout_complete: outcome.stdout_complete,
                stderr_complete: outcome.stderr_complete,
                log: None,
            }),
        }),
    }
}

#[cfg(test)]
mod tests;
