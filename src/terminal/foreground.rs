//! Synchronous terminal attachment plus the app-owned suspend/resume boundary.
//! Only the process supervisor signals/waits for the child. These operations
//! never install signal dispositions or hold a temporary mask across an await.

use super::{ForegroundLease, Session};
use crate::error::ConfctlError;
use ::nix::sys::signal::{SigSet, SigmaskHow, Signal, pthread_sigmask};
use rustix::{
    process::Pid,
    termios::{OptionalActions, Termios},
};
use std::{io, os::fd::OwnedFd, sync::Arc};
use tokio::sync::oneshot;
use tokio_util::sync::CancellationToken;

pub struct ForegroundSuspendRequest {
    pub tty: OwnedFd,
    pub expected_foreground: Pid,
    pub cancellation: CancellationToken,
    pub reply: oneshot::Sender<Result<ForegroundResume, ConfctlError>>,
}

pub struct ForegroundResume {
    pub foreground: Pid,
}

struct Saved {
    foreground: Pid,
    termios: Termios,
}

pub struct ForegroundAttachment {
    session: Arc<Session>,
    child: Pid,
    saved: Option<Saved>,
    child_termios: Option<Termios>,
    active: bool,
}

impl ForegroundLease {
    /// Group ownership is checked by the supervisor immediately before this.
    pub fn attach(&mut self, child: Pid) -> Result<ForegroundAttachment, ConfctlError> {
        self._lease.session.broker.check_available()?;
        let session = self._lease.session.clone();
        let saved = if session.controlling_tty {
            let foreground = rustix::termios::tcgetpgrp(&session.terminal)
                .map_err(|error| terminal_error("read foreground process group", error.into()))?;
            if foreground != rustix::process::getpgrp() {
                return Err(terminal_error(
                    "attach foreground child",
                    io::Error::other("confctl does not own the controlling terminal"),
                ));
            }
            Some(Saved {
                foreground,
                termios: rustix::termios::tcgetattr(&session.terminal).map_err(|error| {
                    terminal_error("save foreground terminal attributes", error.into())
                })?,
            })
        } else {
            None
        };
        let mut attachment = ForegroundAttachment {
            session,
            child,
            saved,
            child_termios: None,
            active: false,
        };
        attachment.reattach()?;
        Ok(attachment)
    }
}

impl ForegroundAttachment {
    pub fn restore(mut self) -> Result<(), ConfctlError> {
        self.restore_inner()
    }

    pub fn reattach(&mut self) -> Result<(), ConfctlError> {
        let Some(saved) = &self.saved else {
            return Ok(());
        };
        let foreground = rustix::termios::tcgetpgrp(&self.session.terminal).map_err(|error| {
            terminal_error("verify resumed foreground process group", error.into())
        })?;
        if foreground != saved.foreground {
            return Err(terminal_error(
                "attach foreground child",
                io::Error::other("application has not regained foreground ownership"),
            ));
        }
        let result = with_ttou_blocked(|| {
            // Mark cleanup necessary before the first possible mutation.
            self.active = true;
            rustix::termios::tcsetpgrp(&self.session.terminal, self.child)
                .map_err(|error| terminal_error("attach foreground process group", error.into()))?;
            if let Some(attributes) = &self.child_termios {
                rustix::termios::tcsetattr(
                    &self.session.terminal,
                    OptionalActions::Now,
                    attributes,
                )
                .map_err(|error| {
                    terminal_error("restore resumed child terminal attributes", error.into())
                })?;
            }
            Ok(())
        });
        if let Err(primary) = result {
            let cleanup = self.restore_inner().err().into_iter().collect();
            return Err(primary.with_secondary(cleanup));
        }
        Ok(())
    }

    /// Restore first. The app owns fresh continuation observation and self-stop
    /// policy; a successful signal send by itself can never grant this reply.
    pub async fn request_suspend(
        &mut self,
        cancellation: &CancellationToken,
    ) -> Result<ForegroundResume, ConfctlError> {
        // Read the stopped child's modes before giving the terminal back. A
        // failed snapshot still attempts parent restoration in the same order.
        let snapshot = if self.active && self.saved.is_some() {
            rustix::termios::tcgetattr(&self.session.terminal)
                .map(|attributes| self.child_termios = Some(attributes))
                .map_err(|error| {
                    terminal_error("save stopped child terminal attributes", error.into())
                })
        } else {
            Ok(())
        };
        let restored = self.restore_inner();
        match (snapshot, restored) {
            (Ok(()), Ok(())) => {}
            (Err(primary), Err(secondary)) => return Err(primary.with_secondary(vec![secondary])),
            (Err(error), _) | (_, Err(error)) => return Err(error),
        }
        let saved = self.saved.as_ref().ok_or_else(|| {
            terminal_error(
                "suspend foreground child",
                io::Error::other("no controlling terminal"),
            )
        })?;
        let sender = self
            .session
            .suspend
            .lock()
            .unwrap_or_else(|error| error.into_inner())
            .clone()
            .ok_or_else(|| {
                terminal_error(
                    "suspend foreground child",
                    io::Error::other("application suspend policy is unavailable"),
                )
            })?;
        let tty = self
            .session
            .terminal
            .try_clone()
            .map_err(|error| terminal_error("duplicate controlling terminal", error))?;
        let (reply, response) = oneshot::channel();
        let request = ForegroundSuspendRequest {
            tty,
            expected_foreground: saved.foreground,
            cancellation: cancellation.clone(),
            reply,
        };
        tokio::select! {
            result = sender.send(request) => result.map_err(|_| terminal_error("request foreground suspension", io::Error::other("application suspend owner closed")))?,
            _ = cancellation.cancelled() => return Err(ConfctlError::Cancelled),
        }
        let resumed = tokio::select! {
            result = response => result.map_err(|_| terminal_error("await foreground resume", io::Error::other("application resume reply lost")))??,
            _ = cancellation.cancelled() => return Err(ConfctlError::Cancelled),
        };
        let actual = rustix::termios::tcgetpgrp(&self.session.terminal)
            .map_err(|error| terminal_error("verify foreground resume", error.into()))?;
        if resumed.foreground != saved.foreground || actual != saved.foreground {
            return Err(terminal_error(
                "verify foreground resume",
                io::Error::other("foreground ownership was not restored"),
            ));
        }
        Ok(resumed)
    }

    pub(crate) fn owns_terminal(&self) -> Result<bool, ConfctlError> {
        if self.saved.is_none() {
            return Ok(false);
        }
        rustix::termios::tcgetpgrp(&self.session.terminal)
            .map(|foreground| foreground == self.child)
            .map_err(|error| {
                terminal_error("verify attached child foreground ownership", error.into())
            })
    }

    pub(crate) fn controlling_tty(&self) -> bool {
        self.saved.is_some()
    }

    fn restore_inner(&mut self) -> Result<(), ConfctlError> {
        if !self.active {
            return Ok(());
        }
        self.active = false;
        let saved = self
            .saved
            .as_ref()
            .expect("active attachment has saved terminal state");
        with_ttou_blocked(|| {
            let mut errors = Vec::new();
            if let Err(error) = rustix::termios::tcsetpgrp(&self.session.terminal, saved.foreground)
            {
                errors.push(terminal_error(
                    "restore foreground process group",
                    error.into(),
                ));
            }
            if let Err(error) = rustix::termios::tcsetattr(
                &self.session.terminal,
                OptionalActions::Now,
                &saved.termios,
            ) {
                errors.push(terminal_error(
                    "restore foreground terminal attributes",
                    error.into(),
                ));
            }
            if errors.is_empty() {
                Ok(())
            } else {
                Err(errors.remove(0).with_secondary(errors))
            }
        })
    }
}

impl Drop for ForegroundAttachment {
    fn drop(&mut self) {
        if let Err(error) = self.restore_inner() {
            self.session
                .broker
                .unobserved
                .lock()
                .unwrap_or_else(|error| error.into_inner())
                .push(error);
        }
    }
}

fn terminal_error(operation: &'static str, source: io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation,
        path: None,
        source,
    }
}

/// Entire mask lifetime is a synchronous call on one thread. Only SIGTTOU is
/// added; exact previous mask is restored even after the terminal operation fails.
fn with_ttou_blocked(
    operation: impl FnOnce() -> Result<(), ConfctlError>,
) -> Result<(), ConfctlError> {
    let mut guard = TtouMask::block()?;
    let primary = operation();
    let restore = guard.restore();
    match (primary, restore) {
        (Ok(()), result) => result,
        (Err(primary), Ok(())) => Err(primary),
        (Err(primary), Err(secondary)) => Err(primary.with_secondary(vec![secondary])),
    }
}

/// The marker prevents moving the thread-local mask owner to another thread.
struct TtouMask {
    previous: SigSet,
    active: bool,
    _same_thread: std::marker::PhantomData<std::rc::Rc<()>>,
}

impl TtouMask {
    fn block() -> Result<Self, ConfctlError> {
        let mut mask = SigSet::empty();
        mask.add(Signal::SIGTTOU);
        let mut previous = SigSet::empty();
        pthread_sigmask(SigmaskHow::SIG_BLOCK, Some(&mask), Some(&mut previous)).map_err(
            |error| {
                terminal_error(
                    "block terminal job-control signal",
                    io::Error::from_raw_os_error(error as i32),
                )
            },
        )?;
        Ok(Self {
            previous,
            active: true,
            _same_thread: std::marker::PhantomData,
        })
    }

    fn restore(&mut self) -> Result<(), ConfctlError> {
        pthread_sigmask(SigmaskHow::SIG_SETMASK, Some(&self.previous), None).map_err(|error| {
            terminal_error(
                "restore thread signal mask",
                io::Error::from_raw_os_error(error as i32),
            )
        })?;
        self.active = false;
        Ok(())
    }
}

impl Drop for TtouMask {
    fn drop(&mut self) {
        if self.active {
            let _ = self.restore();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mask() -> SigSet {
        let mut current = SigSet::empty();
        pthread_sigmask(SigmaskHow::SIG_SETMASK, None, Some(&mut current)).expect("query mask");
        current
    }

    fn same_mask(left: SigSet, right: SigSet) {
        for signal in Signal::iterator() {
            assert_eq!(
                left.contains(signal),
                right.contains(signal),
                "mask for {signal}"
            );
        }
    }

    #[test]
    fn ttou_mask_restores_exact_prior_mask_on_success_error_and_unwind() {
        let before = mask();
        with_ttou_blocked(|| {
            let during = mask();
            assert!(during.contains(Signal::SIGTTOU));
            for signal in Signal::iterator().filter(|signal| *signal != Signal::SIGTTOU) {
                assert_eq!(during.contains(signal), before.contains(signal));
            }
            Ok(())
        })
        .expect("success");
        same_mask(before, mask());
        let error =
            with_ttou_blocked(|| Err(ConfctlError::Cancelled)).expect_err("operation error");
        assert!(matches!(error, ConfctlError::Cancelled));
        same_mask(before, mask());
        assert!(
            std::panic::catch_unwind(|| {
                let _ = with_ttou_blocked(|| panic!("test unwind"));
            })
            .is_err()
        );
        same_mask(before, mask());
    }
}

#[cfg(test)]
mod pty_tests;
