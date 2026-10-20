//! Concrete input-history mirror setup/log shared by comparisons and lock edits.
//! Callers retain their own metadata, presentation and failure policies.
use crate::{
    app::Context,
    error::ConfctlError,
    events::TaskId,
    nix::process_error,
    process::{
        Captured, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec, TerminationCause,
    },
};
use sha2::{Digest, Sha256};
use std::{
    ffi::{OsStr, OsString},
    io,
    path::{Path, PathBuf},
};

pub(super) enum Log {
    Unavailable(String),
    Failed(ProcessOutcome),
    Ready {
        outcome: ProcessOutcome,
        range: String,
    },
}

#[derive(Clone, Copy)]
pub(super) struct Options {
    pub oneline: bool,
    pub patch: bool,
    pub max_count: Option<usize>,
}

pub(super) async fn log(
    context: &Context,
    task: TaskId,
    url: &str,
    from: &str,
    to: &str,
    options: Options,
) -> Result<Log, ConfctlError> {
    check_cancel(context)?;
    let range = format!("{from}..{to}");
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
            check_cancel(context)?;
            tokio::fs::create_dir_all(&path)
                .await
                .map_err(|source| fs_error("create Git mirror directory", &path, source))?;
            true
        }
        Err(source) => return Err(fs_error("inspect Git mirror directory", &path, source)),
    };
    check_cancel(context)?;
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
    if options.oneline {
        args.push("--oneline".into());
    }
    if options.patch {
        args.push("-p".into());
    }
    if let Some(count) = options.max_count {
        args.push(format!("--max-count={count}").into());
    }
    args.extend(["--end-of-options".into(), range.clone().into(), "--".into()]);
    let outcome = run(context, task, args).await?;
    if !outcome.status.success() {
        return Ok(Log::Failed(outcome));
    }
    Ok(Log::Ready { outcome, range })
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

fn check_cancel(context: &Context) -> Result<(), ConfctlError> {
    if context.cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
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
