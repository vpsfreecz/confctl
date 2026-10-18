//! The qualified no-read GNU logname probe reproduces libc getlogin; Ruby's
//! USER fallback is used only after its exact no-login result. Never use UID or
//! LOGNAME, or resolve the multicall logname symlink into the coreutils entry.

use crate::{
    error::ConfctlError,
    events::TaskId,
    nix::{bytes, process_error},
    process::{
        LoginProbeStdin, ProcessOutcome, ProcessRunner, ProcessScope, ProcessSpec, StdinPolicy,
        TerminationCause,
    },
};
use std::{
    ffi::{OsStr, OsString},
    os::unix::ffi::OsStringExt,
    path::PathBuf,
};
use tokio_util::sync::CancellationToken;

pub struct LoginProbe {
    program: PathBuf,
    user: Option<OsString>,
}
impl LoginProbe {
    pub fn packaged() -> Result<Self, ConfctlError> {
        let program = option_env!("CONFCTL_LOGNAME").ok_or_else(|| {
            ConfctlError::Configuration("pinned logname tool is not configured".into())
        })?;
        Self::new(program.into(), std::env::var_os("USER"))
    }
    pub fn new(program: PathBuf, user: Option<OsString>) -> Result<Self, ConfctlError> {
        if !program.is_absolute() || program.file_name() != Some(OsStr::new("logname")) {
            return Err(ConfctlError::Configuration(
                "login probe requires the pinned absolute logname entry".into(),
            ));
        }
        Ok(Self { program, user })
    }
    pub async fn resolve<P: ProcessRunner>(
        &self,
        runner: &P,
        cancellation: CancellationToken,
    ) -> Result<Option<OsString>, ConfctlError> {
        let mut spec = ProcessSpec::new(&self.program, "read original login identity");
        spec.stdin = StdinPolicy::LoginProbe(LoginProbeStdin::original().map_err(|source| {
            ConfctlError::Io {
                operation: "duplicate original login input descriptor",
                path: None,
                source,
            }
        })?);
        spec.env = vec![
            ("LC_ALL".into(), Some("C".into())),
            ("LANG".into(), Some("C".into())),
        ];
        let result = runner
            .run(spec, ProcessScope::new(cancellation, TaskId(0)))
            .await?;
        if result.termination == TerminationCause::Canceled {
            return Err(process_error(ConfctlError::Cancelled, result));
        }
        if !result.stdout_complete || !result.stderr_complete {
            return Err(process_error(
                ConfctlError::State("login probe capture is incomplete".into()),
                result,
            ));
        }
        let out = match bytes(&result.stdout).await {
            Ok(bytes) => bytes,
            Err(error) => return Err(process_error(error, result)),
        };
        let err = match bytes(&result.stderr).await {
            Ok(bytes) => bytes,
            Err(error) => return Err(process_error(error, result)),
        };
        match interpret(&result, &out, &err, self.user.as_deref()) {
            Ok(login) => Ok(login),
            Err(error) => Err(process_error(error, result)),
        }
    }
}
fn interpret(
    result: &ProcessOutcome,
    out: &[u8],
    err: &[u8],
    user: Option<&OsStr>,
) -> Result<Option<OsString>, ConfctlError> {
    if result.status.success() {
        if !err.is_empty() || !out.ends_with(b"\n") || out.contains(&0) {
            return Err(ConfctlError::State("malformed login probe output".into()));
        }
        return Ok(Some(OsString::from_vec(out[..out.len() - 1].to_vec())));
    }
    if result.code() == Some(1) && out.is_empty() && err == b"logname: no login name\n" {
        return Ok(user.map(OsStr::to_owned));
    }
    Err(ConfctlError::Child {
        description: "read original login identity".into(),
        code: result.code(),
        signal: result.signal(),
        log: None,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::process::Captured;
    use std::{
        os::unix::{ffi::OsStrExt, process::ExitStatusExt},
        time::Instant,
    };
    fn outcome(code: i32) -> ProcessOutcome {
        ProcessOutcome {
            status: std::process::ExitStatus::from_raw(code << 8),
            stdout: Captured::Unavailable,
            stderr: Captured::Unavailable,
            stdout_complete: true,
            stderr_complete: true,
            started: Instant::now(),
            ended: Instant::now(),
            termination: TerminationCause::Exited,
        }
    }
    struct Fake {
        code: i32,
        out: Vec<u8>,
        err: Vec<u8>,
        calls: std::sync::Mutex<Vec<ProcessSpec>>,
    }
    impl ProcessRunner for Fake {
        async fn run(
            &self,
            spec: ProcessSpec,
            _: ProcessScope,
        ) -> Result<ProcessOutcome, ConfctlError> {
            self.calls.lock().unwrap().push(spec);
            let mut result = outcome(self.code);
            result.stdout = Captured::Memory(self.out.clone());
            result.stderr = Captured::Memory(self.err.clone());
            Ok(result)
        }
    }
    #[tokio::test]
    async fn qualified_probe_has_no_args_capture_only_locale_and_original_stdin() {
        let fake = Fake {
            code: 1,
            out: vec![],
            err: b"logname: no login name\n".to_vec(),
            calls: Default::default(),
        };
        let probe = LoginProbe::new(
            "/owned/logname".into(),
            Some(OsString::from_vec(b"f\xffx".to_vec())),
        )
        .unwrap();
        assert_eq!(
            probe
                .resolve(&fake, CancellationToken::new())
                .await
                .unwrap()
                .unwrap()
                .as_bytes(),
            b"f\xffx"
        );
        let calls = fake.calls.lock().unwrap();
        assert_eq!(calls.len(), 1);
        let spec = &calls[0];
        assert_eq!(spec.program, PathBuf::from("/owned/logname"));
        assert!(spec.args.is_empty());
        assert!(matches!(spec.stdin, StdinPolicy::LoginProbe(_)));
        assert_eq!(spec.stdout, crate::process::StreamPolicy::Capture);
        assert_eq!(spec.stderr, crate::process::StreamPolicy::Capture);
        assert_eq!(
            spec.env,
            vec![
                ("LC_ALL".into(), Some("C".into())),
                ("LANG".into(), Some("C".into()))
            ]
        );
    }
    #[tokio::test]
    async fn unqualified_probe_failure_preserves_evidence_without_user_fallback() {
        let fake = Fake {
            code: 23,
            out: b"partial".to_vec(),
            err: b"failed probe".to_vec(),
            calls: Default::default(),
        };
        let probe = LoginProbe::new("/owned/logname".into(), Some("fallback".into())).unwrap();
        let error = probe
            .resolve(&fake, CancellationToken::new())
            .await
            .unwrap_err();
        let ConfctlError::ProcessFailure { source, evidence } = error else {
            panic!("probe evidence lost")
        };
        assert!(matches!(
            *source,
            ConfctlError::Child { code: Some(23), .. }
        ));
        assert_eq!(evidence.stdout.memory(), Some(b"partial".as_slice()));
        assert_eq!(evidence.stderr.memory(), Some(b"failed probe".as_slice()));
    }
    #[test]
    fn successful_native_login_ignores_user_and_removes_exactly_one_newline() {
        assert_eq!(
            interpret(
                &outcome(0),
                b"native\xff\n",
                b"",
                Some(OsStr::new("spoofed"))
            )
            .unwrap()
            .unwrap()
            .as_bytes(),
            b"native\xff"
        );
        assert_eq!(
            interpret(&outcome(0), b"\n", b"", None).unwrap(),
            Some(OsString::new())
        );
        assert_eq!(
            interpret(&outcome(0), b"line\n\n", b"", None)
                .unwrap()
                .unwrap()
                .as_bytes(),
            b"line\n"
        );
    }
    #[test]
    fn only_exact_no_login_failure_permits_user_fallback_including_empty_or_absent() {
        let result = outcome(1);
        assert_eq!(
            interpret(
                &result,
                b"",
                b"logname: no login name\n",
                Some(OsStr::new(""))
            )
            .unwrap(),
            Some(OsString::new())
        );
        assert_eq!(
            interpret(&result, b"", b"logname: no login name\n", None).unwrap(),
            None
        );
        for (code, out, err) in [
            (1, b"".as_slice(), b"different diagnostic\n".as_slice()),
            (2, b"", b"logname: no login name\n"),
            (1, b"partial", b"logname: no login name\n"),
            (0, b"no-newline", b""),
        ] {
            assert!(interpret(&outcome(code), out, err, Some(OsStr::new("fallback"))).is_err());
        }
    }
}
