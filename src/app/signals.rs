//! The application's sole signal context owns self-suspension. A live direct
//! parent in the same session/outside our group is the non-orphan witness.
//! Supported shells keep that parent's SID/PGID stable; arbitrary transient
//! away-and-back group changes are outside this private application contract.

#[cfg(test)]
mod native_tests;

use crate::{
    error::ConfctlError,
    terminal::foreground::{ForegroundResume, ForegroundSuspendRequest},
};
use ::nix::sys::signal::{SigSet, SigmaskHow, Signal, pthread_sigmask, raise};
use rustix::{
    event::{PollFd, PollFlags, Timespec, poll},
    process::{Pid, PidfdFlags, pidfd_open},
};
use std::{os::fd::OwnedFd, time::Duration};
use tokio::{
    signal::unix::{SignalKind, signal},
    sync::mpsc,
};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Debug, Eq, PartialEq)]
struct Identity {
    pid: u32,
    parent: u32,
    group: u32,
    session: u32,
    start: u64,
}
impl Identity {
    fn read(pid: u32) -> Result<Self, ConfctlError> {
        let path = format!("/proc/{pid}/stat");
        let text = std::fs::read_to_string(&path).map_err(|source| ConfctlError::Io {
            operation: "read suspend parent identity",
            path: Some(path.into()),
            source,
        })?;
        Self::parse(&text)
    }
    fn parse(text: &str) -> Result<Self, ConfctlError> {
        let (pid, rest) = text
            .split_once('(')
            .ok_or_else(|| unsupported("invalid process identity"))?;
        let (_, tail) = rest
            .rsplit_once(')')
            .ok_or_else(|| unsupported("invalid process identity"))?;
        let fields: Vec<_> = tail.split_whitespace().collect();
        let field = |index: usize| {
            fields
                .get(index)
                .and_then(|v| v.parse::<u64>().ok())
                .ok_or_else(|| unsupported("incomplete process identity"))
        };
        Ok(Self {
            pid: pid
                .trim()
                .parse()
                .map_err(|_| unsupported("invalid process id"))?,
            parent: u32::try_from(field(1)?).map_err(|_| unsupported("invalid parent id"))?,
            group: u32::try_from(field(2)?).map_err(|_| unsupported("invalid process group"))?,
            session: u32::try_from(field(3)?).map_err(|_| unsupported("invalid session"))?,
            start: field(19)?,
        })
    }
}

struct Witness {
    parent: Identity,
    own: Identity,
    fd: OwnedFd,
}
impl Witness {
    fn establish() -> Result<Self, ConfctlError> {
        let own = Identity::read(std::process::id())?;
        let parent = Identity::read(own.parent)?;
        require_parent(&own, &parent)?;
        let pid = Pid::from_raw(own.parent as i32).ok_or_else(|| unsupported("no live parent"))?;
        let fd = pidfd_open(pid, PidfdFlags::empty())
            .map_err(|error| unsupported(&format!("parent pidfd unavailable: {error}")))?;
        let after = Identity::read(own.pid)?;
        let parent_after = Identity::read(own.parent)?;
        if after.parent != own.parent
            || !same_identity(&after, &own)
            || !same_identity(&parent_after, &parent)
        {
            return Err(unsupported(
                "parent identity changed during pidfd acquisition",
            ));
        }
        require_parent(&after, &parent_after)?;
        let witness = Self { parent, own, fd };
        witness.check()?;
        Ok(witness)
    }
    fn check(&self) -> Result<(), ConfctlError> {
        let mut polling = [PollFd::new(&self.fd, PollFlags::IN)];
        let ready =
            poll(&mut polling, Some(&Timespec::default())).map_err(|source| ConfctlError::Io {
                operation: "observe suspend parent liveness",
                path: None,
                source: source.into(),
            })?;
        check_parent_readiness(ready, polling[0].revents())?;
        let own = Identity::read(self.own.pid)?;
        let parent = Identity::read(self.parent.pid)?;
        if own.parent != self.parent.pid
            || !same_identity(&own, &self.own)
            || !same_identity(&parent, &self.parent)
        {
            return Err(unsupported("parent witness identity or group changed"));
        }
        require_parent(&own, &parent)
    }
}
fn check_parent_readiness(ready: usize, flags: PollFlags) -> Result<(), ConfctlError> {
    if flags.intersects(PollFlags::NVAL | PollFlags::ERR) || (ready != 0 && flags.is_empty()) {
        return Err(ConfctlError::Io {
            operation: "observe suspend parent liveness",
            path: None,
            source: std::io::Error::other("inconclusive parent pidfd readiness"),
        });
    }
    if flags.intersects(PollFlags::IN | PollFlags::HUP) {
        return Err(unsupported("lost live parent witness"));
    }
    if ready != 0 || !flags.is_empty() {
        return Err(ConfctlError::Io {
            operation: "observe suspend parent liveness",
            path: None,
            source: std::io::Error::other("unexpected parent pidfd readiness"),
        });
    }
    Ok(())
}
fn same_identity(a: &Identity, b: &Identity) -> bool {
    a.pid == b.pid && a.start == b.start && a.group == b.group && a.session == b.session
}
fn require_parent(own: &Identity, parent: &Identity) -> Result<(), ConfctlError> {
    if own.parent != parent.pid || own.session != parent.session || own.group == parent.group {
        return Err(unsupported(
            "requires a live direct parent in the same session outside confctl's process group",
        ));
    }
    Ok(())
}
fn check_stop_disposition() -> Result<(), ConfctlError> {
    let mut mask = SigSet::empty();
    pthread_sigmask(SigmaskHow::SIG_SETMASK, None, Some(&mut mask))
        .map_err(|error| unsupported(&format!("read thread signal mask: {error}")))?;
    if mask.contains(Signal::SIGTSTP) {
        return Err(unsupported("SIGTSTP is blocked on the calling thread"));
    }
    let status =
        std::fs::read_to_string("/proc/self/status").map_err(|source| ConfctlError::Io {
            operation: "read suspend signal disposition",
            path: None,
            source,
        })?;
    let bit = 1_u128 << ((Signal::SIGTSTP as u32) - 1);
    for label in ["SigIgn:", "SigCgt:"] {
        let flags = status
            .lines()
            .find_map(|line| line.strip_prefix(label))
            .and_then(|value| u128::from_str_radix(value.trim(), 16).ok())
            .ok_or_else(|| unsupported("signal disposition unavailable"))?;
        if flags & bit != 0 {
            return Err(unsupported(
                "SIGTSTP must have its default uncaught and unignored disposition",
            ));
        }
    }
    Ok(())
}
fn unsupported(reason: &str) -> ConfctlError {
    ConfctlError::Configuration(format!("foreground self-suspension unavailable: {reason}"))
}

pub(super) struct SignalOwner {
    receiver: mpsc::Receiver<ForegroundSuspendRequest>,
    cancellation: CancellationToken,
    shutdown: CancellationToken,
    interrupt: tokio::signal::unix::Signal,
    continued: tokio::signal::unix::Signal,
}
impl SignalOwner {
    /// Registration completes before operations start. A failed constructor drops
    /// the receiver, so a blocked sender can never depend on an absent service.
    pub(super) fn new(
        receiver: mpsc::Receiver<ForegroundSuspendRequest>,
        cancellation: CancellationToken,
        shutdown: CancellationToken,
    ) -> Result<Self, ConfctlError> {
        let register = |kind| {
            signal(kind).map_err(|source| ConfctlError::Io {
                operation: "register application signal owner",
                path: None,
                source,
            })
        };
        let interrupt = register(SignalKind::interrupt())?;
        let continued = register(SignalKind::from_raw(Signal::SIGCONT as i32))?;
        Ok(Self {
            receiver,
            cancellation,
            shutdown,
            interrupt,
            continued,
        })
    }
    /// Closing and rejecting this channel precedes every operation/process join.
    pub(super) async fn serve(self) -> Result<(), ConfctlError> {
        let Self {
            mut receiver,
            cancellation,
            shutdown,
            mut interrupt,
            mut continued,
        } = self;
        loop {
            tokio::select! {biased;
                _=shutdown.cancelled()=>{reject(&mut receiver);return Ok(());},
                received=interrupt.recv()=>{if received.is_none(){reject(&mut receiver);return Err(unsupported("interrupt observation closed"));}cancellation.cancel();},
                request=receiver.recv()=>{let Some(request)=request else{return Ok(());};
                    let result=if request.reply.is_closed(){Err(ConfctlError::Cancelled)}else{suspend(&request,&mut interrupt,&mut continued,&cancellation,&shutdown).await};
                    if let Err(Err(error))=request.reply.send(result){reject(&mut receiver);return Err(error);}
                },
                _=continued.recv()=>{},
            }
        }
    }
}
fn reject(receiver: &mut mpsc::Receiver<ForegroundSuspendRequest>) {
    receiver.close();
    while let Ok(request) = receiver.try_recv() {
        let _ = request.reply.send(Err(ConfctlError::Cancelled));
    }
}

async fn suspend(
    request: &ForegroundSuspendRequest,
    interrupt: &mut tokio::signal::unix::Signal,
    continued: &mut tokio::signal::unix::Signal,
    cancellation: &CancellationToken,
    shutdown: &CancellationToken,
) -> Result<ForegroundResume, ConfctlError> {
    if request.reply.is_closed()
        || request.cancellation.is_cancelled()
        || shutdown.is_cancelled()
        || cancellation.is_cancelled()
    {
        return Err(ConfctlError::Cancelled);
    }
    check_stop_disposition()?;
    let witness = Witness::establish()?;
    if request.expected_foreground != rustix::process::getpgrp() {
        return Err(unsupported("unexpected application foreground group"));
    }
    if rustix::termios::tcgetpgrp(&request.tty)
        .map_err(|error| unsupported(&format!("read controlling foreground group: {error}")))?
        != request.expected_foreground
    {
        return Err(unsupported("terminal was not restored before self-stop"));
    }
    // Discard pre-request continuations. The default unblocked thread-directed
    // raise is synchronous; after it returns, require fresh continuation AND
    // actual foreground ownership while the parent witness remains valid.
    loop {
        tokio::select! {biased;received=continued.recv()=>{if received.is_none(){return Err(unsupported("continuation observation closed"));}},_=std::future::ready(())=>break,}
    }
    witness.check()?;
    check_stop_disposition()?;
    #[cfg(test)]
    native_tests::before_raise(&witness.fd)?;
    raise(Signal::SIGTSTP)
        .map_err(|error| unsupported(&format!("request application stop: {error}")))?;
    let mut observed_continue = false;
    let mut interval = tokio::time::interval(Duration::from_millis(100));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {biased;
            _=shutdown.cancelled()=>return Err(ConfctlError::Cancelled),
            _=cancellation.cancelled()=>return Err(ConfctlError::Cancelled),
            _=request.cancellation.cancelled()=>return Err(ConfctlError::Cancelled),
            received=interrupt.recv()=>{if received.is_none(){return Err(unsupported("interrupt observation closed"));}cancellation.cancel();return Err(ConfctlError::Cancelled);},
            received=continued.recv()=>{if received.is_none(){return Err(unsupported("continuation observation closed"));}observed_continue=true;},
            _=interval.tick()=>{},
        }
        if request.reply.is_closed() {
            return Err(ConfctlError::Cancelled);
        }
        witness.check()?;
        let foreground = rustix::termios::tcgetpgrp(&request.tty)
            .map_err(|error| unsupported(&format!("observe resumed foreground group: {error}")))?;
        if observed_continue && foreground == request.expected_foreground {
            // Recheck the owned pidfd immediately before granting ownership.
            // Parent death is detected on readiness wakes, never by an expiry.
            witness.check()?;
            return Ok(ForegroundResume { foreground });
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::sync::oneshot;

    #[test]
    fn pidfd_readiness_never_grants_after_parent_death_or_inconclusive_poll() {
        assert!(check_parent_readiness(0, PollFlags::empty()).is_ok());
        for flags in [
            PollFlags::IN,
            PollFlags::HUP,
            PollFlags::NVAL,
            PollFlags::ERR,
            PollFlags::empty(),
        ] {
            assert!(check_parent_readiness(1, flags).is_err());
        }
    }

    fn request() -> (
        ForegroundSuspendRequest,
        oneshot::Receiver<Result<ForegroundResume, ConfctlError>>,
    ) {
        let (reply, received) = oneshot::channel();
        (
            ForegroundSuspendRequest {
                tty: std::fs::File::open("/dev/null").unwrap().into(),
                expected_foreground: rustix::process::getpgrp(),
                cancellation: CancellationToken::new(),
                reply,
            },
            received,
        )
    }

    #[tokio::test]
    async fn shutdown_rejects_queued_request_and_releases_blocked_sender_before_join() {
        let (sender, receiver) = mpsc::channel(1);
        let shutdown = CancellationToken::new();
        let owner = SignalOwner::new(receiver, CancellationToken::new(), shutdown.clone()).unwrap();
        let (queued, reply) = request();
        assert!(sender.send(queued).await.is_ok());
        let (blocked, blocked_reply) = request();
        let blocked = tokio::spawn(async move { sender.send(blocked).await.is_err() });
        tokio::task::yield_now().await;
        shutdown.cancel();
        owner.serve().await.unwrap();
        assert!(matches!(reply.await.unwrap(), Err(ConfctlError::Cancelled)));
        assert!(blocked.await.unwrap());
        assert!(blocked_reply.await.is_err());
    }

    #[tokio::test]
    async fn canceled_request_is_rejected_without_querying_or_stopping_a_terminal() {
        let (sender, receiver) = mpsc::channel(1);
        let shutdown = CancellationToken::new();
        let owner = SignalOwner::new(receiver, CancellationToken::new(), shutdown.clone()).unwrap();
        let (request, reply) = request();
        request.cancellation.cancel();
        let service = tokio::spawn(owner.serve());
        assert!(sender.send(request).await.is_ok());
        assert!(matches!(reply.await.unwrap(), Err(ConfctlError::Cancelled)));
        shutdown.cancel();
        service.await.unwrap().unwrap();
    }

    #[test]
    fn calling_thread_blocked_stop_is_explicitly_refused_and_mask_restored() {
        let mut blocked = SigSet::empty();
        blocked.add(Signal::SIGTSTP);
        let mut original = SigSet::empty();
        pthread_sigmask(SigmaskHow::SIG_BLOCK, Some(&blocked), Some(&mut original)).unwrap();
        let result = check_stop_disposition();
        // No assertion or async yield occurs before exact calling-thread restore.
        pthread_sigmask(SigmaskHow::SIG_SETMASK, Some(&original), None).unwrap();
        assert!(
            result
                .unwrap_err()
                .to_string()
                .contains("blocked on the calling thread")
        );
    }

    #[test]
    fn parent_witness_requires_same_session_outside_current_group() {
        let own = Identity {
            pid: 12,
            parent: 10,
            group: 12,
            session: 1,
            start: 7,
        };
        let mut parent = Identity {
            pid: 10,
            parent: 1,
            group: 10,
            session: 1,
            start: 6,
        };
        assert!(require_parent(&own, &parent).is_ok());
        parent.group = 12;
        assert!(require_parent(&own, &parent).is_err());
        parent.group = 10;
        parent.session = 2;
        assert!(require_parent(&own, &parent).is_err());
    }
    #[test]
    fn process_identity_parser_handles_parentheses_and_spaces_in_comm() {
        let mut fields = vec!["S", "10", "12", "1"];
        fields.extend(std::iter::repeat_n("0", 15));
        fields.push("77");
        let identity =
            Identity::parse(&format!("12 (name ) with spaces) {}", fields.join(" "))).unwrap();
        assert_eq!(identity.parent, 10);
        assert_eq!(identity.start, 77);
    }
}
