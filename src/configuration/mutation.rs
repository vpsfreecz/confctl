//! One persistent advisory lock for mutations of a canonical configuration.
//! Top-level callers acquire once; subordinate writers borrow the same guard.
//! Old Ruby writers do not participate in this lock protocol.

use super::ConfigRoot;
use crate::error::ConfctlError;
use rustix::fs::{FlockOperation, Mode, OFlags};
use std::{
    fs::{self, File},
    io,
    os::unix::fs::MetadataExt,
    path::Path,
    time::Duration,
};
use tokio_util::sync::CancellationToken;

pub struct MutationLock;

/// Non-cloneable, without descriptor export. Closing this owned description
/// releases its flock; the persistent pathname is never removed or replaced.
pub struct MutationGuard {
    root: ConfigRoot,
    _root_directory: File,
    _state_directory: File,
    _file: File,
}
impl MutationGuard {
    pub fn require_root(&self, root: &ConfigRoot) -> Result<(), ConfctlError> {
        if self.root.path() != root.path() {
            return Err(ConfctlError::Configuration(
                "mutation guard belongs to a different configuration root".into(),
            ));
        }
        require_inode(root.path(), &self._root_directory, true)?;
        require_inode(&root.path().join(".confctl"), &self._state_directory, true)?;
        require_inode(
            &root.path().join(".confctl/mutation.lock"),
            &self._file,
            false,
        )
    }
}
impl MutationLock {
    /// Construction of read-only routes never calls this function. Acquisition
    /// creates only the private directory/lock, then waits interruptibly without
    /// a blocking OS call, background worker, expiry, or implicit reentrancy.
    pub async fn acquire(
        root: &ConfigRoot,
        cancellation: &CancellationToken,
    ) -> Result<MutationGuard, ConfctlError> {
        if cancellation.is_cancelled() {
            return Err(ConfctlError::Cancelled);
        }
        let directory = File::from(
            rustix::fs::open(
                root.path(),
                OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            )
            .map_err(|source| failure("open mutation root", root.path(), source.into()))?,
        );
        // mkdirat/openat keep both entries anchored to the opened root. A
        // symlink or a non-directory .confctl cannot redirect the lock outside.
        match rustix::fs::mkdirat(&directory, ".confctl", Mode::from_raw_mode(0o777)) {
            Ok(()) | Err(rustix::io::Errno::EXIST) => {}
            Err(source) => {
                return Err(failure(
                    "create mutation state directory",
                    root.path(),
                    source.into(),
                ));
            }
        }
        let state = File::from(
            rustix::fs::openat(
                &directory,
                ".confctl",
                OFlags::RDONLY | OFlags::DIRECTORY | OFlags::NOFOLLOW | OFlags::CLOEXEC,
                Mode::empty(),
            )
            .map_err(|source| {
                failure("open mutation state directory", root.path(), source.into())
            })?,
        );
        let path = root.path().join(".confctl/mutation.lock");
        let descriptor = rustix::fs::openat(
            &state,
            "mutation.lock",
            OFlags::RDWR | OFlags::CREATE | OFlags::NOFOLLOW | OFlags::CLOEXEC | OFlags::NONBLOCK,
            Mode::from_raw_mode(0o666),
        )
        .map_err(|source| failure("open mutation lock", &path, source.into()))?;
        let file = File::from(descriptor);
        if !file
            .metadata()
            .map_err(|source| failure("inspect mutation lock", &path, source))?
            .is_file()
        {
            return Err(ConfctlError::Configuration(
                "mutation lock must be a regular file".into(),
            ));
        }
        loop {
            if cancellation.is_cancelled() {
                return Err(ConfctlError::Cancelled);
            }
            match rustix::fs::flock(&file, FlockOperation::NonBlockingLockExclusive) {
                Ok(()) => {
                    if cancellation.is_cancelled() {
                        return Err(ConfctlError::Cancelled);
                    }
                    let guard = MutationGuard {
                        root: root.clone(),
                        _root_directory: directory,
                        _state_directory: state,
                        _file: file,
                    };
                    guard.require_root(root)?;
                    return Ok(guard);
                }
                Err(rustix::io::Errno::AGAIN | rustix::io::Errno::INTR) => {}
                Err(source) => return Err(failure("acquire mutation lock", &path, source.into())),
            }
            tokio::select! {biased;
                _=cancellation.cancelled()=>return Err(ConfctlError::Cancelled),
                _=tokio::time::sleep(Duration::from_millis(25))=>{},
            }
        }
    }
}
fn require_inode(path: &Path, held: &File, directory: bool) -> Result<(), ConfctlError> {
    let current = fs::symlink_metadata(path)
        .map_err(|source| failure("verify mutation lock path", path, source))?;
    let saved = held
        .metadata()
        .map_err(|source| failure("verify owned mutation descriptor", path, source))?;
    if (directory && !current.is_dir())
        || (!directory && !current.is_file())
        || current.dev() != saved.dev()
        || current.ino() != saved.ino()
    {
        return Err(ConfctlError::State(
            "configuration mutation lock/root was replaced".into(),
        ));
    }
    Ok(())
}
fn failure(operation: &'static str, path: &Path, source: io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation,
        path: Some(path.into()),
        source,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        fs,
        os::unix::fs::{MetadataExt, symlink},
        process::{Child, Command},
        time::Instant,
    };

    #[tokio::test]
    async fn canceled_acquisition_does_not_create_configuration_state() {
        let directory = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(directory.path()).unwrap();
        let cancellation = CancellationToken::new();
        cancellation.cancel();
        assert!(matches!(
            MutationLock::acquire(&root, &cancellation).await,
            Err(ConfctlError::Cancelled)
        ));
        assert!(!directory.path().join(".confctl").exists());
    }
    #[tokio::test]
    async fn canceled_waiter_and_dropped_guard_preserve_the_same_lock_inode() {
        let directory = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(directory.path()).unwrap();
        let first = MutationLock::acquire(&root, &CancellationToken::new())
            .await
            .unwrap();
        let path = directory.path().join(".confctl/mutation.lock");
        fs::write(&path, b"persistent contents").unwrap();
        let inode = fs::metadata(&path).unwrap().ino();
        let cancellation = CancellationToken::new();
        let stop = cancellation.clone();
        let other = root.clone();
        let second = tokio::spawn(async move { MutationLock::acquire(&other, &stop).await });
        tokio::time::sleep(Duration::from_millis(40)).await;
        assert!(!second.is_finished());
        cancellation.cancel();
        assert!(matches!(
            second.await.unwrap(),
            Err(ConfctlError::Cancelled)
        ));
        drop(first);
        let third = MutationLock::acquire(&root, &CancellationToken::new())
            .await
            .unwrap();
        assert_eq!(fs::metadata(&path).unwrap().ino(), inode);
        assert_eq!(fs::read(path).unwrap(), b"persistent contents");
        drop(third);
    }
    #[tokio::test]
    async fn lock_directory_and_file_symlinks_cannot_escape_the_root() {
        let directory = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(directory.path()).unwrap();
        symlink(outside.path(), directory.path().join(".confctl")).unwrap();
        assert!(
            MutationLock::acquire(&root, &CancellationToken::new())
                .await
                .is_err()
        );
        assert!(!outside.path().join("mutation.lock").exists());
        fs::remove_file(directory.path().join(".confctl")).unwrap();
        fs::create_dir(directory.path().join(".confctl")).unwrap();
        fs::write(outside.path().join("target"), b"unchanged").unwrap();
        symlink(
            outside.path().join("target"),
            directory.path().join(".confctl/mutation.lock"),
        )
        .unwrap();
        assert!(
            MutationLock::acquire(&root, &CancellationToken::new())
                .await
                .is_err()
        );
        assert_eq!(
            fs::read(outside.path().join("target")).unwrap(),
            b"unchanged"
        );
    }
    #[tokio::test]
    async fn wrong_root_or_replaced_lock_cannot_authorize_feature_publication() {
        use crate::configuration::{ConfigRelativePath, FilePolicy, StateFiles};
        let first = tempfile::tempdir().unwrap();
        let second = tempfile::tempdir().unwrap();
        fs::create_dir(first.path().join("data")).unwrap();
        fs::create_dir(second.path().join("data")).unwrap();
        let root = ConfigRoot::new(first.path()).unwrap();
        let other = ConfigRoot::new(second.path()).unwrap();
        let guard = MutationLock::acquire(&root, &CancellationToken::new())
            .await
            .unwrap();
        let path = ConfigRelativePath::new("data/state.json").unwrap();
        assert!(
            StateFiles::new(other)
                .replace(&guard, &path, b"{}", FilePolicy::KernelState)
                .is_err()
        );
        assert!(!second.path().join("data/state.json").exists());
        assert!(!second.path().join(".confctl").exists());
        // Explicit noncooperating replacement fault: the old inode remains
        // locked, but its guard must never authorize a write under the new one.
        fs::rename(
            first.path().join(".confctl/mutation.lock"),
            first.path().join("old-lock"),
        )
        .unwrap();
        fs::write(first.path().join(".confctl/mutation.lock"), b"replacement").unwrap();
        assert!(
            StateFiles::new(root)
                .replace(&guard, &path, b"{}", FilePolicy::KernelState)
                .is_err()
        );
        assert!(!first.path().join("data/state.json").exists());
    }
    #[tokio::test]
    async fn replacement_root_at_the_same_path_does_not_match_the_guard() {
        use crate::configuration::{ConfigRelativePath, FilePolicy, StateFiles};
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("configuration");
        fs::create_dir(&path).unwrap();
        let root = ConfigRoot::new(&path).unwrap();
        let guard = MutationLock::acquire(&root, &CancellationToken::new())
            .await
            .unwrap();
        fs::rename(&path, directory.path().join("old-configuration")).unwrap();
        fs::create_dir(&path).unwrap();
        fs::create_dir(path.join("data")).unwrap();
        let replacement = ConfigRoot::new(&path).unwrap();
        let state = StateFiles::new(replacement);
        assert!(
            state
                .replace(
                    &guard,
                    &ConfigRelativePath::new("data/state.json").unwrap(),
                    b"{}",
                    FilePolicy::KernelState
                )
                .is_err()
        );
        assert!(!path.join("data/state.json").exists());
        assert!(!path.join(".confctl").exists());
    }
    struct OwnedChild(Child);
    impl Drop for OwnedChild {
        fn drop(&mut self) {
            if self.0.try_wait().ok().flatten().is_none() {
                let _ = self.0.kill();
            }
            let _ = self.0.wait();
        }
    }
    #[tokio::test]
    async fn independent_process_lock_blocks_and_cancellation_releases_only_the_waiter() {
        const ENV: &str = "CONFCTL_MUTATION_LOCK_CHILD_ROOT";
        if let Some(path) = std::env::var_os(ENV) {
            let root = ConfigRoot::new(&path).unwrap();
            let guard = MutationLock::acquire(&root, &CancellationToken::new())
                .await
                .unwrap();
            fs::write(root.path().join("held"), b"").unwrap();
            let started = Instant::now();
            while !root.path().join("release").exists() {
                assert!(started.elapsed() < Duration::from_secs(5));
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
            drop(guard);
            return;
        }
        let directory = tempfile::tempdir().unwrap();
        let root = ConfigRoot::new(directory.path()).unwrap();
        let mut child=OwnedChild(Command::new(std::env::current_exe().unwrap()).args(["--exact","configuration::mutation::tests::independent_process_lock_blocks_and_cancellation_releases_only_the_waiter","--nocapture"]).env(ENV,directory.path()).spawn().unwrap());
        let started = Instant::now();
        while !root.path().join("held").exists() {
            assert!(started.elapsed() < Duration::from_secs(3));
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        let cancellation = CancellationToken::new();
        let stop = cancellation.clone();
        let other = root.clone();
        let waiter = tokio::spawn(async move { MutationLock::acquire(&other, &stop).await });
        tokio::time::sleep(Duration::from_millis(40)).await;
        assert!(!waiter.is_finished());
        cancellation.cancel();
        assert!(matches!(
            waiter.await.unwrap(),
            Err(ConfctlError::Cancelled)
        ));
        fs::write(root.path().join("release"), b"").unwrap();
        loop {
            if let Some(status) = child.0.try_wait().unwrap() {
                assert!(status.success());
                break;
            }
            assert!(started.elapsed() < Duration::from_secs(5));
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
        let guard = MutationLock::acquire(&root, &CancellationToken::new())
            .await
            .unwrap();
        drop(guard);
    }
}
