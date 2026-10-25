//! Reliable operation events. Raw bytes and their stream are never normalized.
//!
//! Delivery completes only when the sole consumer acknowledges the event. A
//! repaint may coalesce progress, but that policy belongs to the presentation
//! model: this transport does not discard output or task transitions.

use crate::error::ConfctlError;
use std::path::PathBuf;
use tokio::sync::{mpsc, oneshot};

#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct TaskId(pub u64);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum OutputStream {
    Stdout,
    Stderr,
}

impl OutputStream {
    pub const fn name(self) -> &'static str {
        match self {
            Self::Stdout => "stdout",
            Self::Stderr => "stderr",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Style {
    Plain,
    Heading,
    Machine,
    Generation,
    Success,
    Failure,
    Incomplete,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TaskState {
    Succeeded,
    Failed,
    Skipped,
    Incomplete,
    Canceled,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum TableLayout {
    #[default]
    Auto,
    Columns,
    Rows,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum TableAlignment {
    #[default]
    Left,
    Right,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TableFormat {
    pub layout: TableLayout,
    pub header: bool,
    pub align: Vec<TableAlignment>,
}

impl Default for TableFormat {
    fn default() -> Self {
        Self {
            layout: TableLayout::Auto,
            header: true,
            align: Vec::new(),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ActivityCount {
    pub observed: u64,
    pub total: Option<u64>,
}

#[derive(Debug, PartialEq)]
pub enum UiEvent {
    /// The consumer acknowledges only after it parks any live repaint.
    TerminalState(crate::terminal::TerminalState),
    /// Only the active prompt lease may bypass parked background output.
    PromptFrame {
        preview: Vec<UiEvent>,
        text: String,
    },
    /// Prelude contains only Table/Diagnostic values, validated before dedup.
    OperationGroup {
        group: u64,
        description: String,
        prelude: Vec<UiEvent>,
    },
    Output {
        task: TaskId,
        stream: OutputStream,
        bytes: Vec<u8>,
    },
    ActivityOutput {
        task: TaskId,
        stream: OutputStream,
        bytes: Vec<u8>,
    },
    /// Compatibility bytes required only by the plain presentation.
    PlainOutput {
        stream: OutputStream,
        bytes: Vec<u8>,
    },
    CommandStarting {
        task: TaskId,
        display: String,
    },
    BuildProgress {
        task: TaskId,
        building: ActivityCount,
        fetching: ActivityCount,
    },
    Table {
        stream: OutputStream,
        columns: Vec<String>,
        rows: Vec<Vec<String>>,
        /// Original unescaped values; empty/missing entries use Plain.
        cell_styles: Vec<Vec<Style>>,
        format: TableFormat,
    },
    /// Admitted logical work whose execution body has not been entered.
    TaskPending {
        task: TaskId,
        description: String,
    },
    /// Logical execution has begun; this does not assert native child liveness.
    TaskStarted {
        task: TaskId,
        description: String,
    },
    TaskProgress {
        task: TaskId,
        completed: u64,
        total: Option<u64>,
        detail: String,
    },
    TaskFinished {
        task: TaskId,
        state: TaskState,
    },
    LogAvailable {
        task: TaskId,
        path: PathBuf,
    },
    Diagnostic {
        stream: OutputStream,
        style: Style,
        text: String,
    },
}

#[derive(Clone, Debug)]
pub struct EventSink {
    sender: mpsc::Sender<Delivery>,
}

pub struct EventReceiver {
    receiver: mpsc::Receiver<Delivery>,
}

#[derive(Debug)]
pub struct Delivery {
    pub event: UiEvent,
    acknowledgement: oneshot::Sender<Result<(), String>>,
}

pub struct DeliveryAcknowledgement {
    acknowledgement: oneshot::Sender<Result<(), String>>,
}

impl DeliveryAcknowledgement {
    pub fn acknowledge(self, result: Result<(), String>) {
        let _ = self.acknowledgement.send(result);
    }
}

impl Delivery {
    /// Move the event's owned bytes without cloning a failed current chunk.
    pub fn into_parts(self) -> (UiEvent, DeliveryAcknowledgement) {
        (
            self.event,
            DeliveryAcknowledgement {
                acknowledgement: self.acknowledgement,
            },
        )
    }
    /// Acknowledge only after the event's required output/log effects succeed.
    pub fn acknowledge(self, result: Result<(), String>) {
        let _ = self.acknowledgement.send(result);
    }
}

impl EventSink {
    pub fn channel(capacity: usize) -> (Self, EventReceiver) {
        let (sender, receiver) = mpsc::channel(capacity.max(1));
        (Self { sender }, EventReceiver { receiver })
    }

    pub async fn emit(&self, event: UiEvent) -> Result<(), ConfctlError> {
        let (acknowledgement, response) = oneshot::channel();
        self.sender
            .send(Delivery {
                event,
                acknowledgement,
            })
            .await
            .map_err(|_| ConfctlError::EventDelivery("consumer closed".into()))?;
        response
            .await
            .map_err(|_| ConfctlError::EventDelivery("event was not acknowledged".into()))?
            .map_err(ConfctlError::EventDelivery)
    }
}

impl EventReceiver {
    pub async fn receive(&mut self) -> Option<Delivery> {
        self.receiver.recv().await
    }

    pub fn close(&mut self) {
        self.receiver.close();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn sender_waits_for_consumer_and_preserves_bytes_and_stream() {
        let (sink, mut receiver) = EventSink::channel(1);
        let task = tokio::spawn(async move {
            sink.emit(UiEvent::Output {
                task: TaskId(7),
                stream: OutputStream::Stderr,
                bytes: vec![0, 255, b'\n'],
            })
            .await
        });
        let delivery = receiver.receive().await.expect("event");
        assert!(!task.is_finished());
        assert_eq!(
            delivery.event,
            UiEvent::Output {
                task: TaskId(7),
                stream: OutputStream::Stderr,
                bytes: vec![0, 255, b'\n']
            }
        );
        delivery.acknowledge(Ok(()));
        task.await.expect("join").expect("delivery");
    }

    #[tokio::test]
    async fn missing_or_failed_acknowledgement_is_not_success() {
        let (sink, mut receiver) = EventSink::channel(1);
        let first = sink.clone();
        let task = tokio::spawn(async move {
            first
                .emit(UiEvent::LogAvailable {
                    task: TaskId(1),
                    path: "log".into(),
                })
                .await
        });
        drop(receiver.receive().await);
        assert!(matches!(
            task.await.expect("join"),
            Err(ConfctlError::EventDelivery(_))
        ));
        let task = tokio::spawn(async move {
            sink.emit(UiEvent::TaskFinished {
                task: TaskId(1),
                state: TaskState::Failed,
            })
            .await
        });
        receiver
            .receive()
            .await
            .expect("event")
            .acknowledge(Err("disk full".into()));
        assert!(
            matches!(task.await.expect("join"), Err(ConfctlError::EventDelivery(message)) if message == "disk full")
        );
    }
}
