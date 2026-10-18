//! All configuration paths retain the invocation's canonical root. No adapter
//! reads process-global cwd again after construction.

mod files;
mod mutation;
mod settings;
pub use files::*;
pub use mutation::*;
pub use settings::*;

use crate::error::ConfctlError;
use std::path::{Path, PathBuf};

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConfigRoot {
    path: PathBuf,
}
impl ConfigRoot {
    pub fn new(path: impl AsRef<Path>) -> Result<Self, ConfctlError> {
        let path = path
            .as_ref()
            .canonicalize()
            .map_err(|source| ConfctlError::Io {
                operation: "resolve configuration root",
                path: Some(path.as_ref().into()),
                source,
            })?;
        if !path.is_dir() {
            return Err(ConfctlError::Configuration(format!(
                "{} is not a directory",
                path.display()
            )));
        }
        Ok(Self { path })
    }
    pub fn path(&self) -> &Path {
        &self.path
    }
    pub fn short_hash(&self) -> String {
        use sha2::{Digest, Sha256};
        let digest = Sha256::digest(self.path.as_os_str().as_encoded_bytes());
        digest[..4]
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect()
    }
    pub fn require_flake(&self) -> Result<(), ConfctlError> {
        if self.path.join("flake.nix").is_file() {
            Ok(())
        } else {
            Err(ConfctlError::Configuration(format!(
                "{} has no flake.nix; migrate software-pin configurations with confctl v3 before using this version (see docs/swpins-to-flakes.md)",
                self.path.display()
            )))
        }
    }
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ConfigEpoch(pub u64);

#[derive(Clone, Debug)]
pub struct EpochValue<T> {
    pub epoch: ConfigEpoch,
    pub value: T,
}
impl<T> EpochValue<T> {
    pub fn require_epoch(&self, current: ConfigEpoch) -> Result<&T, ConfctlError> {
        if current == self.epoch {
            Ok(&self.value)
        } else {
            Err(ConfctlError::State(
                "configuration view is stale after a file change".into(),
            ))
        }
    }
}
