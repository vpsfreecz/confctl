use super::{FlakeLock, InputChange, check_nix_output};
use crate::{
    configuration::{ConfigRoot, MutationGuard},
    error::ConfctlError,
    events::TaskId,
    nix::{bytes, process_error},
    process::{Captured, ProcessRunner, ProcessScope, ProcessSpec, StreamPolicy, TerminationCause},
    timing::Phase,
};
use std::{ffi::OsString, fs, path::Path};
use tokio_util::sync::CancellationToken;

/// Lock mutation only. The operation caller owns summary, optional changelog/Git
/// commit and cache invalidation; these do not run as hidden adapter side effects.
pub struct InputEditor<P> {
    root: ConfigRoot,
    runner: P,
    cancellation: CancellationToken,
}
impl<P: ProcessRunner> InputEditor<P> {
    pub fn new(root: ConfigRoot, runner: P, cancellation: CancellationToken) -> Self {
        Self {
            root,
            runner,
            cancellation,
        }
    }
    pub async fn update(
        &self,
        guard: &MutationGuard,
        inputs: &[String],
    ) -> Result<Vec<InputChange>, ConfctlError> {
        self.check(guard)?;
        self.root.require_flake()?;
        require_inputs(inputs)?;
        let path = self.root.path().join("flake.lock");
        let old = match FlakeLock::load(&path) {
            Ok(lock) => Some(lock),
            Err(ConfctlError::Io { source, .. })
                if source.kind() == std::io::ErrorKind::NotFound =>
            {
                None
            }
            Err(error) => return Err(error),
        };
        let mut args = vec!["flake".into(), "update".into()];
        args.extend(inputs.iter().map(OsString::from));
        self.run_nix(guard, args).await?;
        let new = FlakeLock::load(&path)?;
        Ok(InputChange::between(old.as_ref(), &new, inputs))
    }
    pub async fn set(
        &self,
        guard: &MutationGuard,
        inputs: &[String],
        revision: &str,
    ) -> Result<Vec<InputChange>, ConfctlError> {
        self.check(guard)?;
        self.root.require_flake()?;
        require_inputs(inputs)?;
        let path = self.root.path().join("flake.lock");
        let old = match FlakeLock::load(&path) {
            Err(ConfctlError::Io { source, .. })
                if source.kind() == std::io::ErrorKind::NotFound =>
            {
                return Err(ConfctlError::Configuration(
                    "flake.lock missing; run confctl inputs update first".into(),
                ));
            }
            result => result?,
        };
        // Validate every selected input before the first child or lock mutation.
        let prepared = inputs
            .iter()
            .map(|input| {
                Ok((
                    input.clone(),
                    old.override_url(input, revision)?,
                    old.original(input),
                ))
            })
            .collect::<Result<Vec<_>, ConfctlError>>()?;
        for (input, url, _) in &prepared {
            self.check(guard)?;
            let temporary = temporary_lock(self.root.path())?;
            // Nix creates its output file with the caller's normal umask, as in
            // Ruby. TempPath retains cleanup ownership after that creation.
            fs::remove_file(&temporary)
                .map_err(|source| io("prepare Nix lock output", &temporary, source))?;
            self.run_nix(
                guard,
                vec![
                    "flake".into(),
                    "lock".into(),
                    "--update-input".into(),
                    input.into(),
                    "--override-input".into(),
                    input.into(),
                    url.into(),
                    "--output-lock-file".into(),
                    temporary.as_os_str().to_owned(),
                ],
            )
            .await?;
            let mut new = FlakeLock::load(&temporary)?;
            for (name, _, original) in &prepared {
                new.restore_original(name, original.as_ref())?;
            }
            self.check(guard)?;
            fs::write(&temporary, new.pretty())
                .map_err(|source| io("restore original input metadata", &temporary, source))?;
            self.check(guard)?;
            fs::rename(&temporary, &path)
                .map_err(|source| io("publish flake.lock", &path, source))?;
            self.check(guard)?;
        }
        let new = FlakeLock::load(&path)?;
        Ok(InputChange::between(Some(&old), &new, inputs))
    }
    fn check(&self, guard: &MutationGuard) -> Result<(), ConfctlError> {
        if self.cancellation.is_cancelled() {
            return Err(ConfctlError::Cancelled);
        }
        guard.require_root(&self.root)
    }
    async fn run_nix(
        &self,
        guard: &MutationGuard,
        args: Vec<OsString>,
    ) -> Result<(), ConfctlError> {
        let mut experimental = false;
        loop {
            self.check(guard)?;
            let mut spec = ProcessSpec::new("nix", "nix input lock mutation");
            if experimental {
                spec.args.extend(
                    [
                        "--extra-experimental-features",
                        "nix-command",
                        "--extra-experimental-features",
                        "flakes",
                    ]
                    .map(Into::into),
                );
            }
            spec.args.extend(args.iter().cloned());
            spec.cwd = Some(self.root.path().into());
            spec.stdout = StreamPolicy::CaptureAndForward;
            spec.stderr = StreamPolicy::CaptureAndForward;
            let mut scope = ProcessScope::new(self.cancellation.clone(), TaskId(0));
            scope.phase = Some(Phase::Evaluation);
            let result = self.runner.run(spec, scope).await?;
            if result.termination == TerminationCause::Canceled || self.cancellation.is_cancelled()
            {
                return Err(process_error(ConfctlError::Cancelled, result));
            }
            if let Err(error) = self.check(guard) {
                return Err(process_error(error, result));
            }
            if !result.stdout_complete
                || !result.stderr_complete
                || matches!(result.stdout, Captured::Unavailable)
                || matches!(result.stderr, Captured::Unavailable)
            {
                return Err(process_error(
                    ConfctlError::State("Nix input mutation capture is incomplete".into()),
                    result,
                ));
            }
            let captured = async {
                Ok::<_, ConfctlError>((bytes(&result.stdout).await?, bytes(&result.stderr).await?))
            }
            .await;
            let (out, err) = match captured {
                Ok(streams) => streams,
                Err(e) => return Err(process_error(e, result)),
            };
            let out = String::from_utf8_lossy(&out);
            let err = String::from_utf8_lossy(&err);
            if result.status.success() {
                return check_nix_output(&out, &err).map_err(|error| process_error(error, result));
            }
            let message = format!("{out}\n{err}").to_lowercase();
            if !experimental
                && message.contains("experimental")
                && (message.contains("nix-command") || message.contains("flakes"))
            {
                experimental = true;
                continue;
            }
            return Err(process_error(
                ConfctlError::Child {
                    description: "nix input lock mutation".into(),
                    code: result.code(),
                    signal: result.signal(),
                    log: None,
                },
                result,
            ));
        }
    }
}
fn temporary_lock(root: &Path) -> Result<tempfile::TempPath, ConfctlError> {
    use sha2::{Digest, Sha256};
    let entropy = tempfile::NamedTempFile::new_in(root)
        .map_err(|source| io("reserve lock nonce", root, source))?;
    let digest = Sha256::digest(entropy.path().as_os_str().as_encoded_bytes());
    let nonce = digest[..6]
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    let path = root.join(format!("flake.lock.tmp-{}-{nonce}", std::process::id()));
    let reservation = fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&path)
        .map_err(|source| io("reserve temporary flake lock", &path, source))?;
    drop(reservation);
    tempfile::TempPath::try_from_path(&path)
        .map_err(|source| io("own temporary flake lock", &path, source))
}
fn require_inputs(inputs: &[String]) -> Result<(), ConfctlError> {
    if inputs.is_empty() {
        Err(ConfctlError::Configuration("inputs empty".into()))
    } else {
        Ok(())
    }
}
fn io(operation: &'static str, path: &Path, source: std::io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation,
        path: Some(path.into()),
        source,
    }
}
