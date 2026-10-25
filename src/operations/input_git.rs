//! Full Git mirrors shared by changelogs and immutable ancestry evidence.
use crate::{
    app::Context,
    error::ConfctlError,
    events::TaskId,
    inputs::{Ancestry, GitComparison, full_sha},
    nix::{bytes, process_error},
    process::{
        Captured, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec, StdinPolicy,
        TerminationCause,
    },
};
use rustix::fs::{FlockOperation, flock};
use sha2::{Digest, Sha256};
use std::{
    ffi::{OsStr, OsString},
    fs::{self, File, OpenOptions},
    io,
    os::unix::fs::OpenOptionsExt,
    path::{Path, PathBuf},
    time::Duration,
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

enum Setup {
    Existing,
    Cloned,
    Failed(ProcessOutcome),
}

struct Mirror<'a> {
    context: &'a Context,
    task: TaskId,
    url: &'a str,
    path: PathBuf,
    key: String,
    _lock: File,
}
impl Drop for Mirror<'_> {
    fn drop(&mut self) {
        // An unrelated fork can retain this open file description until exec.
        // Release at the mirror owner's boundary, after its Git children join.
        let _ = flock(&self._lock, FlockOperation::Unlock);
    }
}
impl<'a> Mirror<'a> {
    async fn acquire(
        context: &'a Context,
        task: TaskId,
        url: &'a str,
    ) -> Result<Self, ConfctlError> {
        Self::acquire_in(context, task, url, cache_root()?).await
    }
    async fn acquire_in(
        context: &'a Context,
        task: TaskId,
        url: &'a str,
        base: PathBuf,
    ) -> Result<Self, ConfctlError> {
        check_cancel(context)?;
        let key = Sha256::digest(url.as_bytes())
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<String>();
        fs::create_dir_all(&base)
            .map_err(|_| failure(&key, "Unable to access the Git mirror cache"))?;
        let path = base.join(&key);
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .custom_flags(nix::libc::O_NOFOLLOW | nix::libc::O_NONBLOCK)
            .mode(0o600)
            .open(base.join(format!("{key}.lock")))
            .map_err(|_| failure(&key, "Unable to access the Git mirror cache lock"))?;
        if !file.metadata().is_ok_and(|m| m.is_file()) {
            return Err(failure(&key, "Invalid Git mirror cache lock"));
        }
        loop {
            check_cancel(context)?;
            match flock(&file, FlockOperation::NonBlockingLockExclusive) {
                Ok(()) => break,
                Err(rustix::io::Errno::AGAIN | rustix::io::Errno::INTR) => {}
                Err(_) => return Err(failure(&key, "Unable to lock the Git mirror cache")),
            }
            tokio::select! { biased; _=context.cancellation.cancelled()=>return Err(ConfctlError::Cancelled), _=tokio::time::sleep(Duration::from_millis(25))=>{} }
        }
        Ok(Self {
            context,
            task,
            url,
            path,
            key,
            _lock: file,
        })
    }
    async fn ensure_repository(&self) -> Result<Setup, ConfctlError> {
        match fs::symlink_metadata(&self.path) {
            Ok(_) => {
                self.validate(&self.path).await?;
                return Ok(Setup::Existing);
            }
            Err(e) if e.kind() == io::ErrorKind::NotFound => {}
            Err(_) => return Err(failure(&self.key, "Unable to inspect the Git mirror cache")),
        }
        let temporary = tempfile::Builder::new()
            .prefix(&format!(".{}-", self.key))
            .tempdir_in(self.path.parent().unwrap())
            .map_err(|_| failure(&self.key, "Unable to prepare the Git mirror cache"))?;
        let candidate = temporary.path().join("mirror");
        let cloned = self
            .run(
                vec![
                    "clone".into(),
                    "--mirror".into(),
                    "--quiet".into(),
                    "--".into(),
                    self.url.into(),
                    candidate.as_os_str().into(),
                ],
                None,
            )
            .await?;
        if !cloned.status.success() {
            return Ok(Setup::Failed(cloned));
        }
        self.validate(&candidate).await?;
        if fs::symlink_metadata(&self.path).is_ok() {
            return Err(failure(
                &self.key,
                "Git mirror cache changed during initialization",
            ));
        }
        check_cancel(self.context)?;
        fs::rename(&candidate, &self.path)
            .map_err(|_| failure(&self.key, "Unable to install the Git mirror cache"))?;
        Ok(Setup::Cloned)
    }
    async fn validate(&self, path: &Path) -> Result<(), ConfctlError> {
        let dir = |p: &Path| fs::symlink_metadata(p).is_ok_and(|m| m.is_dir());
        if !dir(path)
            || !dir(&path.join("objects"))
            || !fs::symlink_metadata(path.join("config")).is_ok_and(|m| m.is_file())
        {
            return Err(failure(
                &self.key,
                "Invalid Git mirror cache; a self-contained bare repository is required",
            ));
        }
        for entry in [
            "shallow",
            "info/grafts",
            "objects/info/alternates",
            "objects/info/http-alternates",
            "commondir",
        ] {
            match fs::symlink_metadata(path.join(entry)) {
                Ok(_) => {
                    return Err(failure(
                        &self.key,
                        "Git mirror cache has shallow, grafted or external object history",
                    ));
                }
                Err(e) if e.kind() == io::ErrorKind::NotFound => {}
                Err(_) => return Err(failure(&self.key, "Unable to inspect Git mirror history")),
            }
        }
        no_object_links(&path.join("objects")).map_err(|_| {
            failure(
                &self.key,
                "Git mirror cache must use self-contained object storage",
            )
        })?;
        let prefix = || vec!["--git-dir".into(), path.as_os_str().into()];
        let mut args = prefix();
        args.extend(["rev-parse".into(), "--is-bare-repository".into()]);
        let bare = self.required(args, None).await?;
        let mut args = prefix();
        args.extend(
            [
                "config",
                "--local",
                "--no-includes",
                "--get-all",
                "remote.origin.url",
            ]
            .map(Into::into),
        );
        let origins = self.required(args, None).await?;
        if bare != b"true\n" || origins != format!("{}\n", self.url).as_bytes() {
            return Err(failure(
                &self.key,
                "Git mirror cache is not bare or has an unexpected origin",
            ));
        }
        let mut args = prefix();
        args.extend(["config", "--get-regexp", "^(extensions\\.(partialclone|worktreeconfig)|remote\\..*\\.(promisor|partialclonefilter|vcs))$"].map(Into::into));
        let outcome = self.run(args, None).await?;
        let out = bytes(&outcome.stdout).await?;
        if outcome.code() != Some(1) || !out.is_empty() {
            return Err(process_error(
                failure(
                    &self.key,
                    "Git mirror cache has unsupported partial or repository configuration",
                ),
                outcome,
            ));
        }
        Ok(())
    }
    fn args(&self, args: &[&str]) -> Vec<OsString> {
        let mut out = vec!["--git-dir".into(), self.path.as_os_str().into()];
        out.extend(args.iter().map(OsString::from));
        out
    }
    async fn commit_present(&self, rev: &str) -> Result<bool, ConfctlError> {
        let out = self
            .required(
                self.args(&["cat-file", "--batch-check=%(objectname) %(objecttype)"]),
                Some(format!("{rev}\n").into_bytes()),
            )
            .await?;
        if out == format!("{rev} missing\n").as_bytes() {
            return Ok(false);
        }
        if out == format!("{rev} commit\n").as_bytes() {
            return Ok(true);
        }
        Err(failure(
            &self.key,
            "Git ancestry comparison requires exact commit objects",
        ))
    }
    async fn ensure_commit(&self, rev: &str) -> Result<(), ConfctlError> {
        if !self.commit_present(rev).await? {
            self.required(
                self.args(&[
                    "fetch",
                    "--no-auto-maintenance",
                    "--no-show-forced-updates",
                    "--no-tags",
                    "--quiet",
                    "origin",
                    rev,
                ]),
                None,
            )
            .await?;
            if !self.commit_present(rev).await? {
                return Err(failure(
                    &self.key,
                    "Git mirror does not contain the requested commit after fetching",
                ));
            }
        }
        Ok(())
    }
    async fn ancestor(&self, from: &str, to: &str) -> Result<bool, ConfctlError> {
        let result = self
            .run(self.args(&["merge-base", "--is-ancestor", from, to]), None)
            .await?;
        match result.code() {
            Some(0) => Ok(true),
            Some(1) => Ok(false),
            _ => Err(process_error(
                failure(&self.key, "Git failed while checking commit ancestry"),
                result,
            )),
        }
    }
    async fn required(
        &self,
        args: Vec<OsString>,
        input: Option<Vec<u8>>,
    ) -> Result<Vec<u8>, ConfctlError> {
        let result = self.run(args, input).await?;
        if !result.status.success() {
            return Err(process_error(
                failure(&self.key, "Git mirror operation failed"),
                result,
            ));
        }
        bytes(&result.stdout)
            .await
            .map_err(|e| process_error(e, result))
    }
    async fn run(
        &self,
        args: Vec<OsString>,
        input: Option<Vec<u8>>,
    ) -> Result<ProcessOutcome, ConfctlError> {
        check_cancel(self.context)?;
        self.context.keep_log();
        let mut spec = ProcessSpec::new("git", "read input repository history");
        // Child-only proof settings: callers and their hooks retain their environment.
        for key in [
            "GIT_DIR",
            "GIT_WORK_TREE",
            "GIT_COMMON_DIR",
            "GIT_INDEX_FILE",
            "GIT_OBJECT_DIRECTORY",
            "GIT_ALTERNATE_OBJECT_DIRECTORIES",
            "GIT_GRAFT_FILE",
            "GIT_SHALLOW_FILE",
            "GIT_NAMESPACE",
            "GIT_REPLACE_REF_BASE",
            "GIT_CONFIG",
        ] {
            spec.env.push((key.into(), None));
        }
        for key in ["GIT_NO_REPLACE_OBJECTS", "GIT_NO_LAZY_FETCH"] {
            spec.env.push((key.into(), Some("1".into())));
        }
        spec.args = [
            "--no-replace-objects",
            "-c",
            "core.commitGraph=false",
            "-c",
            "gc.auto=0",
            "-c",
            "maintenance.auto=false",
        ]
        .map(OsString::from)
        .into();
        spec.args.extend(args);
        spec.cwd = Some(self.context.cwd.clone());
        if let Some(input) = input {
            spec.stdin = StdinPolicy::Bytes(input);
        }
        let outcome = self
            .context
            .processes
            .run(
                spec,
                ProcessScope::new(self.context.cancellation.clone(), self.task),
            )
            .await?;
        if outcome.termination == TerminationCause::Canceled
            || self.context.cancellation.is_cancelled()
        {
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
}
fn no_object_links(path: &Path) -> io::Result<()> {
    for entry in fs::read_dir(path)? {
        let entry = entry?;
        let kind = entry.file_type()?;
        if kind.is_symlink() {
            return Err(io::Error::other("symlinked object storage"));
        }
        if kind.is_dir() {
            no_object_links(&entry.path())?;
        }
    }
    Ok(())
}
fn failure(key: &str, message: &str) -> ConfctlError {
    ConfctlError::Configuration(format!("{message} (cache {key})"))
}
fn check_cancel(context: &Context) -> Result<(), ConfctlError> {
    if context.cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}

pub(super) async fn compare(
    context: &Context,
    comparison: GitComparison,
) -> Result<Ancestry, ConfctlError> {
    if !full_sha(&comparison.old_rev) || !full_sha(&comparison.new_rev) {
        return Err(ConfctlError::Configuration(
            "Git comparison requires full commit SHAs".into(),
        ));
    }
    let url = format!(
        "https://github.com/{}/{}",
        comparison.owner, comparison.repo
    );
    compare_url(context, &url, &comparison.old_rev, &comparison.new_rev).await
}
async fn compare_url(
    context: &Context,
    url: &str,
    old: &str,
    new: &str,
) -> Result<Ancestry, ConfctlError> {
    if old == new && full_sha(old) {
        return Ok(Ancestry::Ahead);
    }
    let mirror = Mirror::acquire(context, TaskId(0), url).await?;
    if let Setup::Failed(outcome) = mirror.ensure_repository().await? {
        return Err(process_error(
            failure(&mirror.key, "Git mirror operation failed"),
            outcome,
        ));
    }
    mirror.ensure_commit(old).await?;
    mirror.ensure_commit(new).await?;
    mirror.validate(&mirror.path).await?;
    if mirror.ancestor(old, new).await? {
        return Ok(Ancestry::Ahead);
    }
    mirror
        .required(
            mirror.args(&["rev-list", "--quiet", "--parents", old, new, "--"]),
            None,
        )
        .await?;
    Ok(if mirror.ancestor(new, old).await? {
        Ancestry::Behind
    } else {
        Ancestry::Diverged
    })
}
pub(super) async fn log(
    context: &Context,
    task: TaskId,
    url: &str,
    from: &str,
    to: &str,
    options: Options,
) -> Result<Log, ConfctlError> {
    let mirror = match Mirror::acquire(context, task, url).await {
        Ok(m) => m,
        Err(ConfctlError::Configuration(s)) => return Ok(Log::Unavailable(s)),
        Err(e) => return Err(e),
    };
    let cloned = match mirror.ensure_repository().await {
        Ok(Setup::Cloned) => true,
        Ok(Setup::Existing) => false,
        Ok(Setup::Failed(outcome)) => return Ok(Log::Failed(outcome)),
        Err(ConfctlError::Configuration(s)) => return Ok(Log::Unavailable(s)),
        Err(e) => return Err(e),
    };
    if !cloned {
        let outcome = mirror
            .run(
                mirror.args(&[
                    "fetch",
                    "--no-auto-maintenance",
                    "--no-show-forced-updates",
                    "--quiet",
                    "origin",
                ]),
                None,
            )
            .await?;
        if !outcome.status.success() {
            return Ok(Log::Failed(outcome));
        }
    }
    mirror.validate(&mirror.path).await?;
    let range = format!("{from}..{to}");
    let mut args = mirror.args(&["log", "--no-decorate", "--left-right", "--cherry-mark"]);
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
    let outcome = mirror.run(args, None).await?;
    if !outcome.status.success() {
        Ok(Log::Failed(outcome))
    } else {
        Ok(Log::Ready { outcome, range })
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

#[cfg(test)]
mod lock_tests {
    use super::*;
    use crate::{events::EventSink, process::Supervisor, terminal::Ui, timing::Timing};
    use std::future::Future;
    use tokio_util::sync::CancellationToken;

    #[tokio::test]
    async fn url_lock_wait_is_cancellable_without_initializing_or_removing_shared_state() {
        let directory = tempfile::tempdir().unwrap();
        let (events, mut receiver) = EventSink::channel(64);
        let cancel = CancellationToken::new();
        let ui = Ui::new(std::io::stdin(), events.clone(), cancel.clone(), None).unwrap();
        let supervisor = Supervisor::new(
            events.clone(),
            directory.path().into(),
            1024,
            Duration::from_millis(30),
            None,
            None,
        );
        let context = Context::for_test(
            directory.path().into(),
            cancel.clone(),
            events,
            supervisor,
            ui,
            Timing::start(),
            directory.path().join("log"),
        );
        // Use a URL unique to this fixture; no repository setup occurs here.
        let url = directory.path().to_str().unwrap();
        let first = Mirror::acquire_in(&context, TaskId(0), url, directory.path().join("cache"))
            .await
            .unwrap();
        {
            let waiter =
                Mirror::acquire_in(&context, TaskId(0), url, directory.path().join("cache"));
            tokio::pin!(waiter);
            assert!(
                std::future::poll_fn(|cx| std::task::Poll::Ready(
                    waiter.as_mut().poll(cx).is_pending()
                ))
                .await
            );
            cancel.cancel();
            assert!(matches!(waiter.await, Err(ConfctlError::Cancelled)));
        }
        assert!(!first.path.exists());
        let lock_path = first.path.with_extension("lock");
        assert!(lock_path.is_file());
        // A duplicate shares flock ownership like a descriptor inherited by an
        // unrelated fork. It must not extend the mirror guard's lock lifetime.
        let inherited = first._lock.try_clone().unwrap();
        let lock = OpenOptions::new()
            .read(true)
            .write(true)
            .open(&lock_path)
            .unwrap();
        assert_eq!(
            flock(&lock, FlockOperation::NonBlockingLockExclusive),
            Err(rustix::io::Errno::AGAIN),
            "cancelling a waiter must not release the owner's lock"
        );
        drop(first);
        flock(&lock, FlockOperation::NonBlockingLockExclusive).unwrap();
        assert!(inherited.metadata().unwrap().is_file());
        drop(inherited);
        context.processes.shutdown().await.unwrap();
        context.ui.shutdown().unwrap();
        drop(context);
        receiver.close();
        while let Some(delivery) = receiver.receive().await {
            delivery.acknowledge(Ok(()));
        }
    }
}
