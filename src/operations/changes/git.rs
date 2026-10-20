//! Ruby-compatible per-role mirror effects, using native argv and existing captures.
use super::{Format, chosen_url};
use crate::{
    app::Context,
    error::ConfctlError,
    events::TaskId,
    nix::process_error,
    operations::pager::{Pager, WrittenOrClosed},
    process::{
        Captured, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec, TerminationCause,
    },
};
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::{
    ffi::{OsStr, OsString},
    io,
    path::{Path, PathBuf},
};
use tokio::io::AsyncReadExt;

pub(super) enum Log {
    Unavailable(String),
    Failed(ProcessOutcome),
    Ready {
        outcome: ProcessOutcome,
        range: String,
    },
}

pub(super) async fn log(
    context: &Context,
    target: Option<&Value>,
    deployed: Option<&Value>,
    format: Format<'_>,
    task: TaskId,
) -> Result<Log, ConfctlError> {
    super::check_cancel(context)?;
    let Some(url) = valid(chosen_url(target, deployed)) else {
        return Ok(Log::Unavailable("missing or invalid repository URL".into()));
    };
    let (Some(mut old), Some(mut new)) = (
        valid(deployed.and_then(|info| info.get("rev"))),
        valid(target.and_then(|info| info.get("rev"))),
    ) else {
        return Ok(Log::Unavailable("missing or invalid revision".into()));
    };
    if format.downgrade {
        std::mem::swap(&mut old, &mut new);
    }
    let range = format!("{old}..{new}");
    let digest = Sha256::digest(url.as_bytes())
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    let path = cache_root()?.join(digest);
    let clone = match tokio::fs::symlink_metadata(&path).await {
        Ok(metadata) if metadata.file_type().is_dir() => false,
        Ok(_) => {
            return Ok(Log::Unavailable(format!(
                "cache leaf {} is not a real directory",
                path.display()
            )));
        }
        Err(error) if error.kind() == io::ErrorKind::NotFound => {
            super::check_cancel(context)?;
            tokio::fs::create_dir_all(&path)
                .await
                .map_err(|source| fs_error("create Git mirror directory", &path, source))?;
            true
        }
        Err(source) => return Err(fs_error("inspect Git mirror directory", &path, source)),
    };
    super::check_cancel(context)?;
    context.keep_log();
    let setup = if clone {
        vec![
            "clone".into(),
            "--quiet".into(),
            "--mirror".into(),
            "--".into(),
            url.into(),
            path.as_os_str().into(),
        ]
    } else {
        vec![
            "-C".into(),
            path.as_os_str().into(),
            "fetch".into(),
            "--no-show-forced-updates".into(),
            "--quiet".into(),
        ]
    };
    let outcome = run(context, task, setup).await?;
    if !outcome.status.success() {
        return Ok(Log::Failed(outcome));
    }
    let mut args: Vec<OsString> = vec![
        "-C".into(),
        path.as_os_str().into(),
        "log".into(),
        "--no-decorate".into(),
        "--left-right".into(),
        "--cherry-mark".into(),
    ];
    if !format.verbose {
        args.push("--oneline".into());
    }
    if format.patch {
        args.push("-p".into());
    }
    if !format.verbose && !format.patch {
        args.push("--max-count=50".into());
    }
    args.extend(["--end-of-options".into(), range.clone().into(), "--".into()]);
    let outcome = run(context, task, args).await?;
    if !outcome.status.success() {
        return Ok(Log::Failed(outcome));
    }
    Ok(Log::Ready { outcome, range })
}

fn valid(value: Option<&Value>) -> Option<&str> {
    value
        .and_then(Value::as_str)
        .filter(|text| !text.is_empty() && !text.contains('\0'))
}

async fn run(
    context: &Context,
    task: TaskId,
    args: Vec<OsString>,
) -> Result<ProcessOutcome, ConfctlError> {
    if context.cancellation.is_cancelled() {
        return Err(ConfctlError::Cancelled);
    }
    let mut spec = ProcessSpec::new("git", "read input repository history");
    spec.args = args;
    spec.cwd = Some(context.cwd.clone());
    let outcome = context
        .processes
        .run(spec, ProcessScope::new(context.cancellation.clone(), task))
        .await?;
    if outcome.termination == TerminationCause::Canceled || context.cancellation.is_cancelled() {
        return Err(process_error(ConfctlError::Cancelled, outcome));
    }
    if !outcome.stdout_complete
        || !outcome.stderr_complete
        || matches!(outcome.stdout, Captured::Unavailable)
        || matches!(outcome.stderr, Captured::Unavailable)
    {
        return Err(process_error(
            ConfctlError::State("Git capture is incomplete".into()),
            outcome,
        ));
    }
    Ok(outcome)
}

pub(super) async fn stream(
    pager: &mut Pager<'_>,
    capture: &Captured,
) -> Result<bool, ConfctlError> {
    match capture {
        Captured::Memory(bytes) => {
            for chunk in bytes.chunks(16 * 1024) {
                if matches!(pager.write(chunk).await?, WrittenOrClosed::Closed) {
                    return Ok(true);
                }
            }
        }
        Captured::Spool { file, length } => {
            let path = file.path();
            let mut file = tokio::fs::File::open(path)
                .await
                .map_err(|source| fs_error("open captured Git output", path, source))?;
            let mut remaining = *length;
            let mut buffer = vec![0; 16 * 1024];
            while remaining > 0 {
                let limit = remaining.min(buffer.len() as u64) as usize;
                let count = file
                    .read(&mut buffer[..limit])
                    .await
                    .map_err(|source| fs_error("read captured Git output", path, source))?;
                if count == 0 {
                    return Err(fs_error(
                        "read captured Git output",
                        path,
                        io::Error::new(io::ErrorKind::UnexpectedEof, "short Git capture"),
                    ));
                }
                remaining -= count as u64;
                if matches!(
                    pager.write(&buffer[..count]).await?,
                    WrittenOrClosed::Closed
                ) {
                    return Ok(true);
                }
            }
        }
        Captured::Unavailable => {
            return Err(ConfctlError::State("Git output was not captured".into()));
        }
    }
    Ok(false)
}

fn cache_root() -> Result<PathBuf, ConfctlError> {
    let xdg = std::env::var_os("XDG_CACHE_HOME");
    let mut home = std::env::var_os("HOME");
    if xdg.is_none() && home.is_none() {
        home = Some(
            nix::unistd::User::from_uid(nix::unistd::getuid())
                .map_err(|source| ConfctlError::Io {
                    operation: "look up current user's home",
                    path: None,
                    source: source.into(),
                })?
                .ok_or_else(|| {
                    ConfctlError::Configuration("current user has no passwd home".into())
                })?
                .dir
                .into_os_string(),
        );
    }
    Ok(cache_base(xdg.as_deref(), home.as_deref()))
}
fn cache_base(xdg: Option<&OsStr>, home: Option<&OsStr>) -> PathBuf {
    // Ruby File.join("", "child") begins at /; an empty set value is not unset.
    let base = |value: &OsStr| {
        if value.is_empty() {
            PathBuf::from("/")
        } else {
            PathBuf::from(value)
        }
    };
    xdg.map(base)
        .unwrap_or_else(|| base(home.expect("resolved current-user home")).join(".cache"))
        .join("confctl/git-mirrors")
}
fn fs_error(operation: &'static str, path: &Path, source: io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation,
        path: Some(path.into()),
        source,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn cache_path_preserves_literal_set_empty_and_unset_environment() {
        assert_eq!(
            cache_base(
                Some(OsStr::new("/fixture/cache")),
                Some(OsStr::new("/unused"))
            ),
            PathBuf::from("/fixture/cache/confctl/git-mirrors")
        );
        assert_eq!(
            cache_base(Some(OsStr::new("")), Some(OsStr::new("/unused"))),
            PathBuf::from("/confctl/git-mirrors")
        );
        assert_eq!(
            cache_base(None, Some(OsStr::new("/fixture/home"))),
            PathBuf::from("/fixture/home/.cache/confctl/git-mirrors")
        );
        assert_eq!(
            cache_base(None, Some(OsStr::new(""))),
            PathBuf::from("/.cache/confctl/git-mirrors")
        );
    }
}
