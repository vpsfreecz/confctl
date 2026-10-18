//! Errors distinguish infrastructure failure from ordinary per-host outcomes.

use std::{ffi::OsString, fmt, io, path::PathBuf};

pub struct NixCommandContext {
    pub cwd: PathBuf,
    pub program: OsString,
    pub args: Vec<OsString>,
}
impl fmt::Debug for NixCommandContext {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("NixCommandContext { <redacted> }")
    }
}
impl fmt::Display for NixCommandContext {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("Nix command")
    }
}
pub struct NixCommandFailure {
    pub context: NixCommandContext,
    pub source: Box<ConfctlError>,
}
impl fmt::Debug for NixCommandFailure {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("NixCommandFailure { <redacted> }")
    }
}
impl fmt::Display for NixCommandFailure {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        fmt::Display::fmt(&self.source, f)
    }
}
impl std::error::Error for NixCommandFailure {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(self.source.as_ref())
    }
}

#[derive(Debug, thiserror::Error)]
pub enum ConfctlError {
    #[error("{0}")]
    NixCommandFailure(#[source] Box<NixCommandFailure>),
    #[error("{0}")]
    Usage(String),
    #[error("{0}")]
    Configuration(String),
    #[error("{0}")]
    Selection(String),
    #[error("{0}")]
    State(String),
    #[error("{context}: {source}")]
    Decode {
        context: String,
        #[source]
        source: serde_json::Error,
    },
    #[error("{operation}: {source}")]
    Io {
        operation: &'static str,
        path: Option<PathBuf>,
        #[source]
        source: io::Error,
    },
    #[error("unable to start {description}: {source}")]
    Spawn {
        /// Redacted description, never unreviewed argv or credentials.
        description: String,
        #[source]
        source: io::Error,
    },
    #[error("unable to drain {stream}: {source}")]
    Drain {
        stream: &'static str,
        #[source]
        source: io::Error,
    },
    /// Keep available process evidence without replacing the primary failure.
    /// The evidence type's Debug implementation must redact captured bytes.
    #[error("{source}")]
    ProcessFailure {
        #[source]
        source: Box<ConfctlError>,
        evidence: Box<crate::process::PartialProcessEvidence>,
    },
    #[error("{source}")]
    OutputFailure {
        #[source]
        source: Box<ConfctlError>,
        pending: Box<crate::presentation::PendingOutput>,
    },
    #[error("event delivery failed: {0}")]
    EventDelivery(String),
    #[error("terminal is already owned by another foreground operation")]
    TerminalBusy,
    #[error("foreground child job control is not available")]
    ForegroundJobControlUnavailable,
    #[error("end of input")]
    InputEof,
    #[error("prompt input exceeds {limit} bytes")]
    InputTooLong { limit: usize },
    #[error("operation canceled")]
    Cancelled,
    #[error("{description} failed (exit {code:?}, signal {signal:?})")]
    Child {
        description: String,
        code: Option<i32>,
        signal: Option<i32>,
        log: Option<PathBuf>,
    },
    #[error("{0}")]
    Health(String),
    #[error("remote outcome is uncertain: {0}")]
    UncertainRemote(String),
    #[error("{primary}")]
    Cleanup {
        #[source]
        primary: Box<ConfctlError>,
        secondary: Vec<ConfctlError>,
    },
}

impl ConfctlError {
    /// Evidence and cleanup wrappers do not change the primary cancellation
    /// disposition. A later canceled cleanup cannot replace an earlier failure.
    pub fn is_cancelled(&self) -> bool {
        match self {
            Self::NixCommandFailure(failure) => failure.source.is_cancelled(),
            Self::Cancelled => true,
            Self::ProcessFailure { source, .. } | Self::OutputFailure { source, .. } => {
                source.is_cancelled()
            }
            Self::Cleanup { primary, .. } => primary.is_cancelled(),
            _ => false,
        }
    }
    /// Preserve the first failure and append cleanup failures in arrival order.
    /// The presentation owner decides how to report the secondary diagnostics.
    pub fn with_secondary(self, mut later: Vec<Self>) -> Self {
        if later.is_empty() {
            return self;
        }
        match self {
            Self::Cleanup {
                primary,
                mut secondary,
            } => {
                secondary.append(&mut later);
                Self::Cleanup { primary, secondary }
            }
            primary => Self::Cleanup {
                primary: Box::new(primary),
                secondary: later,
            },
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nix_wrapper_redacts_command_and_captures_and_keeps_primary_cancellation() {
        use crate::process::{Captured, PartialProcessEvidence};
        let error = ConfctlError::NixCommandFailure(Box::new(NixCommandFailure {
            context: NixCommandContext {
                cwd: "/private-cwd".into(),
                program: "private-program".into(),
                args: vec!["private-argument".into()],
            },
            source: Box::new(ConfctlError::ProcessFailure {
                source: Box::new(ConfctlError::Cancelled),
                evidence: Box::new(PartialProcessEvidence {
                    status: None,
                    stdout: Captured::Memory(b"private-stdout".to_vec()),
                    stderr: Captured::Memory(b"private-stderr".to_vec()),
                    stdout_complete: false,
                    stderr_complete: false,
                    log: None,
                }),
            }),
        }))
        .with_secondary(vec![ConfctlError::State("later cleanup".into())]);
        assert!(error.is_cancelled());
        for rendered in [format!("{error}"), format!("{error:?}")] {
            for private in [
                "private-cwd",
                "private-program",
                "private-argument",
                "private-stdout",
                "private-stderr",
            ] {
                assert!(!rendered.contains(private));
            }
        }
        let ConfctlError::Cleanup { primary, secondary } = error else {
            panic!()
        };
        assert!(primary.is_cancelled());
        assert_eq!(secondary.len(), 1);
    }

    #[test]
    fn wrapped_primary_cancellation_preserves_disposition_without_replacing_prior_failure() {
        use crate::process::{Captured, PartialProcessEvidence};
        let error = ConfctlError::ProcessFailure {
            source: Box::new(ConfctlError::Cancelled.with_secondary(vec![ConfctlError::InputEof])),
            evidence: Box::new(PartialProcessEvidence {
                status: None,
                stdout: Captured::Unavailable,
                stderr: Captured::Unavailable,
                stdout_complete: false,
                stderr_complete: false,
                log: None,
            }),
        };
        assert!(error.is_cancelled());
        assert!(
            !ConfctlError::Health("earlier failure".into())
                .with_secondary(vec![error])
                .is_cancelled()
        );
    }

    #[test]
    fn cleanup_preserves_primary_source_and_secondary_order() {
        let error = ConfctlError::Spawn {
            description: "fixture child".into(),
            source: io::Error::from(io::ErrorKind::NotFound),
        }
        .with_secondary(vec![ConfctlError::InputEof])
        .with_secondary(vec![ConfctlError::EventDelivery("closed".into())]);

        let ConfctlError::Cleanup { primary, secondary } = error else {
            panic!("cleanup context missing");
        };
        let ConfctlError::Spawn { source, .. } = *primary else {
            panic!("primary failure replaced");
        };
        assert_eq!(source.kind(), io::ErrorKind::NotFound);
        assert!(matches!(
            secondary.as_slice(),
            [ConfctlError::InputEof, ConfctlError::EventDelivery(_)]
        ));
    }

    #[test]
    fn process_failure_retains_cleanup_and_redacts_captured_bytes() {
        use crate::process::{Captured, PartialProcessEvidence};
        use std::error::Error;

        let primary = ConfctlError::Child {
            description: "fixture child".into(),
            code: Some(7),
            signal: None,
            log: None,
        }
        .with_secondary(vec![ConfctlError::InputEof])
        .with_secondary(vec![ConfctlError::EventDelivery("closed".into())]);
        let expected = primary.to_string();
        let error = ConfctlError::ProcessFailure {
            source: Box::new(primary),
            evidence: Box::new(PartialProcessEvidence {
                status: None,
                stdout: Captured::Memory(b"protected stdout".to_vec()),
                stderr: Captured::Memory(b"protected stderr".to_vec()),
                stdout_complete: true,
                stderr_complete: false,
                log: None,
            }),
        };

        assert_eq!(error.to_string(), expected);
        assert_eq!(error.source().unwrap().to_string(), expected);
        assert!(!format!("{error:?}").contains("protected"));
        let ConfctlError::ProcessFailure { source, evidence } = error else {
            unreachable!();
        };
        assert_eq!(
            evidence.stdout.memory(),
            Some(b"protected stdout".as_slice())
        );
        assert!(!evidence.stderr_complete);
        let ConfctlError::Cleanup { primary, secondary } = *source else {
            panic!("cleanup context missing");
        };
        assert!(matches!(
            *primary,
            ConfctlError::Child { code: Some(7), .. }
        ));
        assert!(matches!(
            secondary.as_slice(),
            [ConfctlError::InputEof, ConfctlError::EventDelivery(_)]
        ));
    }
}
