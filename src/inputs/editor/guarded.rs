//! A guarded candidate never asks Nix to write the live lock.
use super::*;
use std::{
    fs::{File, OpenOptions},
    os::unix::{
        ffi::OsStringExt,
        fs::{OpenOptionsExt, PermissionsExt},
    },
    path::PathBuf,
};

pub struct GuardedCandidate {
    pub old: FlakeLock,
    pub candidate: FlakeLock,
    snapshot: Snapshot,
    output: PathBuf,
    candidate_bytes: Vec<u8>,
    _temporary: tempfile::TempDir,
    // This lock spans installation and the caller's optional commit/hooks.
    _git_lock: File,
}
struct Snapshot {
    lock: Vec<u8>,
    flake: Vec<u8>,
    head: Vec<u8>,
    index: Vec<u8>,
    staged: Vec<u8>,
    index_path: PathBuf,
    mode: u32,
}
impl<P: ProcessRunner> InputEditor<P> {
    pub async fn prepare_forward(
        &self,
        guard: &MutationGuard,
        inputs: &[String],
    ) -> Result<GuardedCandidate, ConfctlError> {
        self.check(guard)?;
        self.root.require_flake()?;
        require_inputs(inputs)?;
        let lock_path = self.git_path(guard, "confctl-input-update.lock").await?;
        let file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .mode(0o600)
            .custom_flags(nix::libc::O_NOFOLLOW | nix::libc::O_NONBLOCK)
            .open(&lock_path)
            .map_err(|source| io("open guarded update lock", &lock_path, source))?;
        if !file.metadata().is_ok_and(|m| m.is_file()) {
            return Err(config("guarded update lock must be a regular file"));
        }
        rustix::fs::flock(&file, rustix::fs::FlockOperation::NonBlockingLockExclusive).map_err(
            |e| {
                if e == rustix::io::Errno::AGAIN {
                    config("another guarded input update is running")
                } else {
                    io("lock guarded update", &lock_path, e.into())
                }
            },
        )?;
        let path = self.root.path().join("flake.lock");
        let metadata = fs::symlink_metadata(&path)
            .map_err(|source| io("inspect existing flake.lock", &path, source))?;
        if !metadata.is_file() {
            return Err(config(
                "forward-only updates require an existing regular flake.lock",
            ));
        }
        self.git(guard, &["ls-files", "--error-unmatch", "--", "flake.lock"])
            .await?;
        if !self
            .git(guard, &["status", "--porcelain=v1", "--untracked-files=no"])
            .await?
            .is_empty()
        {
            return Err(config(
                "forward-only updates require a clean tracked checkout and index",
            ));
        }
        let index_path = self.git_path(guard, "index").await?;
        let snapshot = Snapshot {
            lock: read(&path)?,
            flake: read(&self.root.path().join("flake.nix"))?,
            head: self.git(guard, &["rev-parse", "HEAD"]).await?,
            staged: self.git(guard, &["ls-files", "--stage", "-z"]).await?,
            index: read(&index_path)?,
            index_path,
            mode: metadata.permissions().mode() & 0o777,
        };
        let old = FlakeLock::decode(&snapshot.lock)?;
        let targets = crate::inputs::LockGraph::new(&old)?.update_inputs(inputs)?;
        let temporary = tempfile::Builder::new()
            .prefix(".confctl-update-")
            .tempdir_in(self.root.path())
            .map_err(|source| io("prepare guarded update", self.root.path(), source))?;
        let reference = temporary.path().join("reference.lock");
        let output = temporary.path().join("candidate.lock");
        fs::write(&reference, &snapshot.lock)
            .map_err(|source| io("write reference lock", &reference, source))?;
        let mut args = vec![
            "flake".into(),
            "update".into(),
            "--reference-lock-file".into(),
            reference.as_os_str().into(),
            "--output-lock-file".into(),
            output.as_os_str().into(),
        ];
        args.extend(targets.iter().map(OsString::from));
        self.run_nix(guard, args).await?;
        if !fs::symlink_metadata(&output).is_ok_and(|m| m.is_file()) {
            return Err(config("Nix did not produce a regular candidate lock"));
        }
        let candidate_bytes = read(&output)?;
        let candidate = FlakeLock::decode(&candidate_bytes)?;
        Ok(GuardedCandidate {
            old,
            candidate,
            snapshot,
            output,
            candidate_bytes,
            _temporary: temporary,
            _git_lock: file,
        })
    }
    pub async fn install_forward(
        &self,
        guard: &MutationGuard,
        prepared: &GuardedCandidate,
        changed: bool,
    ) -> Result<(), ConfctlError> {
        self.check(guard)?;
        let path = self.root.path().join("flake.lock");
        let snapshot = &prepared.snapshot;
        let unchanged = fs::symlink_metadata(&path)
            .is_ok_and(|m| m.is_file() && m.permissions().mode() & 0o777 == snapshot.mode)
            && read(&path)? == snapshot.lock
            && read(&self.root.path().join("flake.nix"))? == snapshot.flake
            && self.git(guard, &["rev-parse", "HEAD"]).await? == snapshot.head
            && self.git(guard, &["ls-files", "--stage", "-z"]).await? == snapshot.staged
            && read(&snapshot.index_path)? == snapshot.index
            && self
                .git(guard, &["status", "--porcelain=v1", "--untracked-files=no"])
                .await?
                .is_empty();
        if !unchanged {
            return Err(config(
                "checkout changed while preparing the input update; retry from a fresh baseline",
            ));
        }
        // The exact candidate bytes inspected by the policy must still own output.
        if !fs::symlink_metadata(&prepared.output).is_ok_and(|m| m.is_file())
            || read(&prepared.output)? != prepared.candidate_bytes
        {
            return Err(config("candidate changed while verifying the input update"));
        }
        self.check(guard)?;
        if changed {
            fs::set_permissions(&prepared.output, fs::Permissions::from_mode(snapshot.mode))
                .map_err(|source| io("preserve flake.lock mode", &prepared.output, source))?;
            fs::rename(&prepared.output, &path)
                .map_err(|source| io("publish validated flake.lock", &path, source))?;
        }
        Ok(())
    }
    async fn git_path(&self, guard: &MutationGuard, name: &str) -> Result<PathBuf, ConfctlError> {
        let mut out = self.git(guard, &["rev-parse", "--git-path", name]).await?;
        if out.last() == Some(&b'\n') {
            out.pop();
        }
        if out.is_empty() || out.contains(&b'\n') {
            return Err(config("guarded update Git path is invalid"));
        }
        let path = PathBuf::from(OsString::from_vec(out));
        Ok(if path.is_absolute() {
            path
        } else {
            self.root.path().join(path)
        })
    }
    async fn git(&self, guard: &MutationGuard, args: &[&str]) -> Result<Vec<u8>, ConfctlError> {
        self.check(guard)?;
        let mut spec = ProcessSpec::new("git", "guarded update Git preflight");
        spec.cwd = Some(self.root.path().into());
        spec.args = args.iter().map(OsString::from).collect();
        spec.env
            .push(("GIT_OPTIONAL_LOCKS".into(), Some("0".into())));
        let result = self
            .runner
            .run(
                spec,
                ProcessScope::new(self.cancellation.clone(), TaskId(0)),
            )
            .await?;
        if result.termination == TerminationCause::Canceled || self.cancellation.is_cancelled() {
            return Err(process_error(ConfctlError::Cancelled, result));
        }
        if !result.status.success() || !result.stdout_complete || !result.stderr_complete {
            return Err(process_error(
                config("guarded update Git preflight failed"),
                result,
            ));
        }
        bytes(&result.stdout)
            .await
            .map_err(|e| process_error(e, result))
    }
}
fn config(message: &str) -> ConfctlError {
    ConfctlError::Configuration(message.into())
}
fn read(path: &Path) -> Result<Vec<u8>, ConfctlError> {
    fs::read(path).map_err(|source| io("read guarded update snapshot", path, source))
}
