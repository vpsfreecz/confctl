use super::{ConfigRoot, MutationGuard};
use crate::error::ConfctlError;
use rustix::fs::{Mode, OFlags};
use std::{
    fs,
    io::{self, Write},
    os::{
        fd::{AsRawFd, OwnedFd},
        unix::fs::PermissionsExt,
    },
    path::{Component, Path, PathBuf},
};

/// Feature destinations only. Generation links and core configuration files use
/// separate state owners and cannot be supplied as generated feature outputs.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConfigRelativePath(PathBuf);
impl ConfigRelativePath {
    pub fn new(path: impl AsRef<Path>) -> Result<Self, ConfctlError> {
        let path = path.as_ref();
        if path.as_os_str().is_empty()
            || path.is_absolute()
            || path
                .components()
                .any(|c| !matches!(c, Component::Normal(_)))
        {
            return Err(ConfctlError::Configuration(
                "feature file must be a relative path without traversal".into(),
            ));
        }
        if ["flake.nix", "flake.lock", "cluster/cluster.nix"]
            .iter()
            .any(|reserved| path == Path::new(reserved))
            || path.starts_with(".confctl")
        {
            return Err(ConfctlError::Configuration(format!(
                "{} is reserved configuration state",
                path.display()
            )));
        }
        Ok(Self(path.into()))
    }
    pub fn as_path(&self) -> &Path {
        &self.0
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FilePolicy {
    KernelState,
    GeneratedNix,
}
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WriteReceipt {
    pub path: PathBuf,
    pub bytes: usize,
}

pub struct StateFiles {
    root: ConfigRoot,
}
impl StateFiles {
    pub fn new(root: ConfigRoot) -> Self {
        Self { root }
    }
    pub fn require_mutation(&self, guard: &MutationGuard) -> Result<(), ConfctlError> {
        guard.require_root(&self.root)
    }
    fn parent(&self, path: &ConfigRelativePath) -> Result<(OwnedFd, PathBuf), ConfctlError> {
        let absolute = self.root.path().join(path.as_path());
        let parent = absolute
            .parent()
            .expect("validated relative file has a parent");
        let canonical = parent
            .canonicalize()
            .map_err(|source| failure("resolve feature parent", parent, source))?;
        if !canonical.starts_with(self.root.path()) {
            return Err(ConfctlError::Configuration(
                "feature parent escapes configuration root".into(),
            ));
        }
        let fd = rustix::fs::open(
            &canonical,
            OFlags::RDONLY | OFlags::DIRECTORY | OFlags::CLOEXEC | OFlags::NOFOLLOW,
            Mode::empty(),
        )
        .map_err(|source| failure("open feature parent", &canonical, source.into()))?;
        // Check the opened object, then retain that descriptor through rename.
        let fd_path = PathBuf::from(format!("/proc/self/fd/{}", fd.as_raw_fd()));
        let opened = fd_path
            .canonicalize()
            .map_err(|source| failure("verify feature parent", &canonical, source))?;
        if !opened.starts_with(self.root.path()) {
            return Err(ConfctlError::Configuration(
                "opened feature parent escapes configuration root".into(),
            ));
        }
        let destination = fd_path.join(absolute.file_name().expect("validated name"));
        if let Ok(metadata) = fs::symlink_metadata(&destination)
            && (metadata.file_type().is_symlink() || !metadata.is_file())
        {
            return Err(ConfctlError::Configuration(format!(
                "{} must be a regular feature file",
                absolute.display()
            )));
        }
        Ok((fd, fd_path))
    }
    pub fn validate(&self, path: &ConfigRelativePath) -> Result<(), ConfctlError> {
        self.parent(path).map(|_| ())
    }
    pub fn read_optional(
        &self,
        path: &ConfigRelativePath,
    ) -> Result<Option<Vec<u8>>, ConfctlError> {
        use std::io::Read;
        let (fd, parent) = self.parent(path)?;
        let name = path.as_path().file_name().expect("validated name");
        let absolute = parent.join(name);
        let opened = match rustix::fs::openat(
            &fd,
            name,
            OFlags::RDONLY | OFlags::NOFOLLOW | OFlags::CLOEXEC | OFlags::NONBLOCK,
            Mode::empty(),
        ) {
            Ok(fd) => fd,
            Err(rustix::io::Errno::NOENT) => return Ok(None),
            Err(source) => return Err(failure("open feature file", &absolute, source.into())),
        };
        let mut file = fs::File::from(opened);
        if !file
            .metadata()
            .map_err(|source| failure("inspect feature file", &absolute, source))?
            .is_file()
        {
            return Err(ConfctlError::Configuration(
                "feature input must be a regular file".into(),
            ));
        }
        let mut bytes = Vec::new();
        file.read_to_end(&mut bytes)
            .map_err(|source| failure("read feature file", &absolute, source))?;
        Ok(Some(bytes))
    }
    pub fn replace(
        &self,
        guard: &MutationGuard,
        path: &ConfigRelativePath,
        bytes: &[u8],
        _policy: FilePolicy,
    ) -> Result<WriteReceipt, ConfctlError> {
        self.require_mutation(guard)?;
        let (parent, fd_path) = self.parent(path)?;
        let name = path.as_path().file_name().expect("validated name");
        let absolute = self.root.path().join(path.as_path());
        let existing = fs::metadata(fd_path.join(name)).ok();
        let mut builder = tempfile::Builder::new();
        builder.prefix(".confctl-new-");
        // Creation goes through open(2), retaining the caller's umask.
        builder.permissions(fs::Permissions::from_mode(0o666));
        let mut temporary = builder
            .tempfile_in(&fd_path)
            .map_err(|source| failure("create feature staging file", &absolute, source))?;
        if let Some(metadata) = existing {
            temporary
                .as_file()
                .set_permissions(metadata.permissions())
                .map_err(|source| failure("preserve feature permissions", &absolute, source))?;
        }
        temporary
            .write_all(bytes)
            .and_then(|()| temporary.as_file().sync_all())
            .map_err(|source| failure("write feature staging file", &absolute, source))?;
        rustix::fs::renameat(
            &parent,
            temporary.path().file_name().expect("staging name"),
            &parent,
            name,
        )
        .map_err(|source| failure("publish feature file", &absolute, source.into()))?;
        rustix::fs::fsync(&parent)
            .map_err(|source| failure("sync feature directory", &absolute, source.into()))?;
        Ok(WriteReceipt {
            path: absolute,
            bytes: bytes.len(),
        })
    }
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
    #[tokio::test]
    async fn validates_parent_and_preserves_existing_file_mode() {
        let scratch = tempfile::tempdir().unwrap();
        fs::create_dir(scratch.path().join("data")).unwrap();
        let root = ConfigRoot::new(scratch.path()).unwrap();
        let guard =
            super::super::MutationLock::acquire(&root, &tokio_util::sync::CancellationToken::new())
                .await
                .unwrap();
        let state = StateFiles::new(root);
        let path = ConfigRelativePath::new("data/kernels.json").unwrap();
        fs::write(scratch.path().join(path.as_path()), b"old").unwrap();
        fs::set_permissions(
            scratch.path().join(path.as_path()),
            fs::Permissions::from_mode(0o640),
        )
        .unwrap();
        state
            .replace(&guard, &path, b"new", FilePolicy::KernelState)
            .unwrap();
        assert_eq!(state.read_optional(&path).unwrap(), Some(b"new".to_vec()));
        assert_eq!(
            fs::metadata(scratch.path().join(path.as_path()))
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o640
        );
    }
    #[test]
    fn refuses_traversal_reserved_files_missing_parent_and_symlink_escape() {
        for path in [
            "../outside",
            "/tmp/outside",
            "flake.nix",
            "flake.lock",
            "cluster/cluster.nix",
            ".confctl/current",
        ] {
            assert!(ConfigRelativePath::new(path).is_err());
        }
        let scratch = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        std::os::unix::fs::symlink(outside.path(), scratch.path().join("escape")).unwrap();
        let state = StateFiles::new(ConfigRoot::new(scratch.path()).unwrap());
        assert!(
            state
                .validate(&ConfigRelativePath::new("escape/out.json").unwrap())
                .is_err()
        );
        assert!(
            state
                .validate(&ConfigRelativePath::new("missing/out.json").unwrap())
                .is_err()
        );
        assert!(!outside.path().join("out.json").exists());
    }
}
