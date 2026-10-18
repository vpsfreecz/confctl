//! One invocation log, owned until joined shutdown and checked finalization.

use super::diagnostic;
use crate::{error::ConfctlError, process::ProcessLog};
use sha2::{Digest, Sha256};
use std::{
    fs::{File, OpenOptions},
    io::{self, Read, Seek, SeekFrom, Write},
    os::unix::fs::{MetadataExt, OpenOptionsExt},
    path::{Path, PathBuf},
};

pub(super) struct InvocationLog {
    file: File,
    path: PathBuf,
}
#[derive(Debug)]
pub(super) struct FinalizationFailure {
    pub(super) source: ConfctlError,
    /// An existing entry for the opened inode, not evidence coverage.
    pub(super) locator: Option<PathBuf>,
}
#[cfg(test)]
static FINALIZATION_FAULT: std::sync::Mutex<Option<PathBuf>> = std::sync::Mutex::new(None);

#[cfg(test)]
pub(super) fn fail_finalization_once(path: PathBuf) {
    let mut fault = FINALIZATION_FAULT.lock().unwrap();
    assert!(fault.is_none());
    *fault = Some(path);
}
impl InvocationLog {
    pub(super) fn open(cwd: &Path, action: &str) -> Result<(Self, ProcessLog), ConfctlError> {
        let root = cwd
            .canonicalize()
            .map_err(|source| failure("resolve invocation log root", Some(cwd), source))?;
        let directory = root.join(".confctl/logs");
        std::fs::create_dir_all(&directory).map_err(|source| {
            failure("create invocation log directory", Some(&directory), source)
        })?;
        let path = directory.join(format!(
            "{}-confctl-{}.log",
            chrono::Local::now().format("%Y-%m-%d--%H-%M-%S"),
            action.replace(' ', "-")
        ));
        let mut file = OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(true)
            .open(&path)
            .map_err(|source| failure("open invocation log", Some(&path), source))?;
        // The action-entry record precedes defaults and command construction.
        // Raw process argv is recorded only by the reviewed fatal formatter.
        writeln!(file, "confctl {action}")
            .and_then(|()| file.flush())
            .map_err(|source| failure("record invocation entry", Some(&path), source))?;
        let writer = file
            .try_clone()
            .map_err(|source| failure("duplicate invocation log", Some(&path), source))?;
        let process = ProcessLog::new(tokio::fs::File::from_std(writer)).with_locator(path.clone());
        Ok((Self { file, path }, process))
    }
    pub(super) fn path(&self) -> &Path {
        &self.path
    }

    /// All Supervisor/log users must have joined and dropped before this call.
    /// Only successful append, sync, close and pathname verification cover the
    /// process captures; a locator/live write alone never authorizes release.
    pub(super) fn finish(
        self,
        error: Option<&ConfctlError>,
        keep: bool,
    ) -> Result<(), FinalizationFailure> {
        let Self { file, path } = self;
        let identity = match file.metadata() {
            Ok(metadata) => (metadata.dev(), metadata.ino()),
            Err(source) => {
                return Err(FinalizationFailure {
                    source: failure("inspect invocation log", Some(&path), source),
                    locator: None,
                });
            }
        };
        let result = (|| {
            #[cfg(test)]
            {
                let mut fault = FINALIZATION_FAULT.lock().unwrap();
                if fault.as_ref() == Some(&path) {
                    fault.take();
                    return Err(failure(
                        "finalize invocation log",
                        Some(&path),
                        io::Error::other("injected finalization failure"),
                    ));
                }
            }
            let seal = finish_storage(file, error)
                .map_err(|source| failure("finalize invocation log", Some(&path), source))?;
            verify_path(&path, identity, &seal).map_err(|source| {
                failure("verify finalized invocation log", Some(&path), source)
            })?;
            if error.is_none() && !keep {
                std::fs::remove_file(&path).map_err(|source| {
                    failure("remove successful invocation log", Some(&path), source)
                })?;
            }
            Ok(())
        })();
        match result {
            Ok(()) => Ok(()),
            Err(source) => {
                // Identity alone justifies a locator, never capture release.
                let locator = std::fs::symlink_metadata(&path)
                    .ok()
                    .filter(|metadata| {
                        metadata.is_file() && (metadata.dev(), metadata.ino()) == identity
                    })
                    .map(|_| path);
                Err(FinalizationFailure { source, locator })
            }
        }
    }
}
fn failure(operation: &'static str, path: Option<&Path>, source: io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation,
        path: path.map(Into::into),
        source,
    }
}

// Private I/O seam tests the actual finalization order, including close errors.
trait LogStorage: Read + Write + Seek {
    fn sync(&self) -> io::Result<()>;
    fn close(self) -> io::Result<()>;
}
impl LogStorage for File {
    fn sync(&self) -> io::Result<()> {
        self.sync_all()
    }
    fn close(self) -> io::Result<()> {
        nix::unistd::close(self).map_err(io::Error::from)
    }
}
struct Seal {
    start: u64,
    length: u64,
    digest: [u8; 32],
}
struct HashedWriter<'a, W> {
    writer: &'a mut W,
    hash: Sha256,
    length: u64,
}
impl<W: Write> Write for HashedWriter<'_, W> {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        let count = self.writer.write(bytes)?;
        self.hash.update(&bytes[..count]);
        self.length += count as u64;
        Ok(count)
    }
    fn flush(&mut self) -> io::Result<()> {
        self.writer.flush()
    }
}
fn finish_storage(mut file: impl LogStorage, error: Option<&ConfctlError>) -> io::Result<Seal> {
    // try_clone shares the live writer offset: always seek after its last join.
    let start = file.seek(SeekFrom::End(0))?;
    let mut writer = HashedWriter {
        writer: &mut file,
        hash: Sha256::new(),
        length: 0,
    };
    if let Some(error) = error {
        diagnostic::fatal_evidence(&mut writer, error)?;
    }
    writer.flush()?;
    let seal = Seal {
        start,
        length: writer.length,
        digest: writer.hash.finalize().into(),
    };
    file.sync()?;
    file.seek(SeekFrom::Start(start))?;
    verify_section(&mut file, &seal)?;
    file.close()?;
    Ok(seal)
}
fn verify_section(reader: &mut impl Read, seal: &Seal) -> io::Result<()> {
    let mut reader = reader.take(seal.length);
    let mut hash = Sha256::new();
    let mut buffer = [0; 16384];
    let mut count = 0;
    loop {
        let length = reader.read(&mut buffer)?;
        if length == 0 {
            break;
        }
        hash.update(&buffer[..length]);
        count += length as u64;
    }
    if count != seal.length || <[u8; 32]>::from(hash.finalize()) != seal.digest {
        return Err(io::Error::other(
            "invocation log evidence verification failed",
        ));
    }
    Ok(())
}
fn verify_path(path: &Path, identity: (u64, u64), seal: &Seal) -> io::Result<()> {
    let mut file = OpenOptions::new()
        .read(true)
        .custom_flags(rustix::fs::OFlags::NOFOLLOW.bits() as i32)
        .open(path)?;
    let metadata = file.metadata()?;
    if !metadata.is_file()
        || (metadata.dev(), metadata.ino()) != identity
        || metadata.len() != seal.start + seal.length
    {
        return Err(io::Error::other(
            "invocation log identity or length changed",
        ));
    }
    file.seek(SeekFrom::Start(seal.start))?;
    let verified = verify_section(&mut file, seal);
    let closed = nix::unistd::close(file).map_err(io::Error::from);
    verified.and(closed)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    struct FaultLog {
        inner: Cursor<Vec<u8>>,
        fault: &'static str,
    }
    impl Read for FaultLog {
        fn read(&mut self, bytes: &mut [u8]) -> io::Result<usize> {
            if self.fault == "read" {
                Err(io::Error::other("read fault"))
            } else {
                self.inner.read(bytes)
            }
        }
    }
    impl Write for FaultLog {
        fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
            if self.fault == "append" {
                Err(io::Error::other("append fault"))
            } else {
                self.inner.write(bytes)
            }
        }
        fn flush(&mut self) -> io::Result<()> {
            if self.fault == "flush" {
                Err(io::Error::other("flush fault"))
            } else {
                Ok(())
            }
        }
    }
    impl Seek for FaultLog {
        fn seek(&mut self, pos: SeekFrom) -> io::Result<u64> {
            self.inner.seek(pos)
        }
    }
    impl LogStorage for FaultLog {
        fn sync(&self) -> io::Result<()> {
            if self.fault == "sync" {
                Err(io::Error::other("sync fault"))
            } else {
                Ok(())
            }
        }
        fn close(self) -> io::Result<()> {
            if self.fault == "close" {
                Err(io::Error::other("close fault"))
            } else {
                Ok(())
            }
        }
    }
    #[test]
    fn append_read_flush_sync_and_close_failures_never_prove_coverage() {
        for fault in ["append", "read", "flush", "sync", "close"] {
            let result = finish_storage(
                FaultLog {
                    inner: Cursor::new(vec![]),
                    fault,
                },
                Some(&ConfctlError::InputEof),
            );
            assert!(result.is_err(), "{fault}");
        }
    }
    #[tokio::test]
    async fn fatal_log_covers_nested_captures_and_labels_partial_streams() {
        use crate::process::{Captured, PartialProcessEvidence};
        let scratch = tempfile::tempdir().unwrap();
        let (log, process) = InvocationLog::open(scratch.path(), "inputs ls").unwrap();
        let path = log.path().to_owned();
        drop(process);
        let error = ConfctlError::ProcessFailure {
            source: Box::new(ConfctlError::InputEof),
            evidence: Box::new(PartialProcessEvidence {
                status: None,
                stdout: Captured::Memory(b"protected primary\0".to_vec()),
                stderr: Captured::Unavailable,
                stdout_complete: false,
                stderr_complete: false,
                log: None,
            }),
        }
        .with_secondary(vec![ConfctlError::ProcessFailure {
            source: Box::new(ConfctlError::Cancelled),
            evidence: Box::new(PartialProcessEvidence {
                status: None,
                stdout: Captured::Unavailable,
                stderr: Captured::Memory(b"protected cleanup\xff".to_vec()),
                stdout_complete: false,
                stderr_complete: true,
                log: None,
            }),
        }]);
        log.finish(Some(&error), false).unwrap();
        let bytes = std::fs::read(path).unwrap();
        assert!(bytes.starts_with(b"confctl inputs ls\n"));
        for expected in [
            b"protected primary\0".as_slice(),
            b"protected cleanup\xff",
            b"[ordered cleanup 1]",
            b"[stdout] complete=false available=true",
        ] {
            assert!(bytes.windows(expected.len()).any(|b| b == expected));
        }
    }
    #[tokio::test]
    async fn success_unlinks_unless_explicitly_kept_and_open_failure_has_no_locator() {
        let scratch = tempfile::tempdir().unwrap();
        let (log, process) = InvocationLog::open(scratch.path(), "ls").unwrap();
        let path = log.path().to_owned();
        drop(process);
        log.finish(None, false).unwrap();
        assert!(!path.exists());
        let (log, process) = InvocationLog::open(scratch.path(), "ls").unwrap();
        let path = log.path().to_owned();
        drop(process);
        log.finish(None, true).unwrap();
        assert!(path.exists());
        let absent = scratch.path().join("absent");
        assert!(InvocationLog::open(&absent, "ls").is_err());
        assert!(!absent.exists());
    }
    #[tokio::test]
    async fn missing_or_replaced_log_is_not_verified() {
        let scratch = tempfile::tempdir().unwrap();
        let (log, process) = InvocationLog::open(scratch.path(), "ls").unwrap();
        let path = log.path().to_owned();
        drop(process);
        std::fs::remove_file(&path).unwrap();
        assert!(
            log.finish(Some(&ConfctlError::InputEof), false)
                .unwrap_err()
                .locator
                .is_none()
        );
        let (log, process) = InvocationLog::open(scratch.path(), "ls").unwrap();
        drop(process);
        std::fs::rename(&path, scratch.path().join("original")).unwrap();
        std::fs::write(&path, b"replacement").unwrap();
        assert!(
            log.finish(Some(&ConfctlError::InputEof), false)
                .unwrap_err()
                .locator
                .is_none()
        );
    }
}
