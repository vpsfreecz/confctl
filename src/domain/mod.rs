//! Shared value types. This module does not select command or signal policy.

mod invocation;
mod selection;

pub use invocation::*;
pub use selection::*;

/// Final invocation state, resolved before a timing report is frozen.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ExitState {
    Completed,
    Failed,
    Canceled,
}

/// Native executable status and its separately determined completion state.
///
/// A failed per-host result does not by itself decide this invocation value.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ExitDisposition {
    pub code: i32,
    pub state: ExitState,
}

impl ExitDisposition {
    pub const fn completed() -> Self {
        Self {
            code: 0,
            state: ExitState::Completed,
        }
    }

    pub const fn failed(code: i32) -> Self {
        Self {
            code,
            state: ExitState::Failed,
        }
    }

    pub const fn canceled(code: i32) -> Self {
        Self {
            code,
            state: ExitState::Canceled,
        }
    }

    pub const fn is_canceled(self) -> bool {
        matches!(self.state, ExitState::Canceled)
    }
}
