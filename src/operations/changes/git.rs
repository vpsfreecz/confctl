//! Comparison metadata policy and pager streaming stay with the comparison owner.
use super::{Format, chosen_url};
pub(super) use crate::operations::input_git::Log;
use crate::{
    app::Context,
    error::ConfctlError,
    events::TaskId,
    operations::{
        input_git,
        pager::{Pager, WrittenOrClosed},
    },
    process::Captured,
};
use serde_json::Value;
use std::{io, path::Path};
use tokio::io::AsyncReadExt;

pub(super) async fn log(
    context: &Context,
    target: Option<&Value>,
    deployed: Option<&Value>,
    format: Format<'_>,
    task: TaskId,
) -> Result<Log, ConfctlError> {
    super::check_cancel(context)?;
    let Some(url) = valid(chosen_url(target, deployed)) else {
        return Ok(Log::Unavailable("missing or invalid repository URL".into()));
    };
    let (Some(mut old), Some(mut new)) = (
        valid(deployed.and_then(|v| v.get("rev"))),
        valid(target.and_then(|v| v.get("rev"))),
    ) else {
        return Ok(Log::Unavailable("missing or invalid revision".into()));
    };
    if format.downgrade {
        std::mem::swap(&mut old, &mut new);
    }
    input_git::log(
        context,
        task,
        url,
        old,
        new,
        input_git::Options {
            oneline: !format.verbose,
            patch: format.patch,
            max_count: (!format.verbose && !format.patch).then_some(50),
        },
    )
    .await
}
fn valid(value: Option<&Value>) -> Option<&str> {
    value
        .and_then(Value::as_str)
        .filter(|text| !text.is_empty() && !text.contains('\0'))
}

pub(super) async fn stream(
    pager: &mut Pager<'_>,
    capture: &Captured,
) -> Result<bool, ConfctlError> {
    match capture {
        Captured::Memory(bytes) => {
            for chunk in bytes.chunks(16 * 1024) {
                if matches!(pager.write(chunk).await?, WrittenOrClosed::Closed) {
                    return Ok(true);
                }
            }
        }
        Captured::Spool { file, length } => {
            let path = file.path();
            let mut file = tokio::fs::File::open(path)
                .await
                .map_err(|source| fs_error("open captured Git output", path, source))?;
            let mut remaining = *length;
            let mut buffer = vec![0; 16 * 1024];
            while remaining > 0 {
                let limit = remaining.min(buffer.len() as u64) as usize;
                let count = file
                    .read(&mut buffer[..limit])
                    .await
                    .map_err(|source| fs_error("read captured Git output", path, source))?;
                if count == 0 {
                    return Err(fs_error(
                        "read captured Git output",
                        path,
                        io::Error::new(io::ErrorKind::UnexpectedEof, "short Git capture"),
                    ));
                }
                remaining -= count as u64;
                if matches!(
                    pager.write(&buffer[..count]).await?,
                    WrittenOrClosed::Closed
                ) {
                    return Ok(true);
                }
            }
        }
        Captured::Unavailable => {
            return Err(ConfctlError::State("Git output was not captured".into()));
        }
    }
    Ok(false)
}

fn fs_error(operation: &'static str, path: &Path, source: io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation,
        path: Some(path.into()),
        source,
    }
}
