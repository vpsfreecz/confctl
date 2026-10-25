//! Ruby input message grouping; only this mutation consumer permits mirror fallback.
use crate::{
    app::Context,
    domain::InputChanges,
    error::ConfctlError,
    events::{OutputStream, Style, TaskId, UiEvent},
    inputs::InputChange,
    nix::{bytes, process_error},
    operations::input_git::{self, Log},
};

pub(super) async fn build(
    ctx: &Context,
    changes: &[InputChange],
    options: &InputChanges,
    setting: bool,
) -> Result<Vec<u8>, ConfctlError> {
    let mut message = title(changes, setting).into_bytes();
    message.extend_from_slice(b"\n\n");
    if !options.changelog {
        for change in changes {
            message.extend(summary(change));
        }
    } else {
        for (i, group) in groups(changes).iter().enumerate() {
            if i > 0 {
                message.extend_from_slice(b"\n\n");
            }
            let mut body = Vec::new();
            for change in group {
                body.extend(summary(change));
            }
            let first = group[0];
            if let (Some(url), Some(old), Some(new)) = (
                url(first),
                first.old_info.as_ref().and_then(|i| i.rev.as_deref()),
                first.new_info.rev.as_deref(),
            ) && old != new
            {
                let (from, to) = if options.downgrade {
                    (new, old)
                } else {
                    (old, new)
                };
                let result = input_git::log(
                    ctx,
                    TaskId(0),
                    url,
                    from,
                    to,
                    input_git::Options {
                        oneline: true,
                        patch: false,
                        max_count: None,
                    },
                )
                .await;
                match result {
                    Ok(Log::Ready { outcome, range }) => {
                        let result = bytes(&outcome.stdout).await.map(|out| {
                            body.extend_from_slice(format!("git log for {range}\n").as_bytes());
                            body.extend_from_slice(strip(&out));
                        });
                        result.map_err(|e| process_error(e, outcome))?;
                    }
                    Ok(Log::Failed(outcome)) => {
                        ctx.keep_log();
                        let text = format!(
                            "Git history failed (exit {:?}, signal {:?}); log {}",
                            outcome.code(),
                            outcome.signal(),
                            ctx.log_path().display()
                        );
                        let delivery = async {
                            let stderr = bytes(&outcome.stderr).await?;
                            if !stderr.is_empty() {
                                ctx.events
                                    .emit(UiEvent::Output {
                                        task: TaskId(0),
                                        stream: OutputStream::Stderr,
                                        bytes: stderr,
                                    })
                                    .await?;
                            }
                            unavailable(ctx, &mut body, &text).await
                        }
                        .await;
                        delivery.map_err(|e| process_error(e, outcome))?;
                    }
                    Ok(Log::Unavailable(text)) => unavailable(ctx, &mut body, &text).await?,
                    Err(e) if cache_failure(&e) => {
                        unavailable(ctx, &mut body, &e.to_string()).await?
                    }
                    Err(e) => return Err(e),
                }
            }
            message.extend_from_slice(trim_end(&body));
        }
    }
    super::canceled(ctx)?;
    Ok(strip(&message).to_vec())
}
async fn unavailable(ctx: &Context, body: &mut Vec<u8>, text: &str) -> Result<(), ConfctlError> {
    super::canceled(ctx)?;
    ctx.keep_log();
    let text = format!("(changelog unavailable: {text})");
    ctx.events
        .emit(UiEvent::Diagnostic {
            stream: OutputStream::Stderr,
            style: Style::Plain,
            text: format!("{text}\n"),
        })
        .await?;
    super::canceled(ctx)?;
    body.extend_from_slice(text.as_bytes());
    Ok(())
}
// Only the cache filesystem boundary may become unavailable. Process/stream IO,
// wrapped evidence, incomplete captures and delivery errors never enter this catch.
fn cache_failure(e: &ConfctlError) -> bool {
    matches!(
        e,
        ConfctlError::Io {
            operation: "look up current user's home"
                | "create Git mirror directory"
                | "inspect Git mirror directory",
            ..
        }
    ) || matches!(e, ConfctlError::Configuration(text) if text == "current user has no passwd home")
        || mirror_unavailable(e)
}
fn mirror_unavailable(e: &ConfctlError) -> bool {
    match e {
        ConfctlError::Configuration(message) => {
            message.starts_with("Git mirror operation failed (cache ")
        }
        ConfctlError::ProcessFailure { source, .. } => mirror_unavailable(source),
        _ => false,
    }
}
fn title(changes: &[InputChange], setting: bool) -> String {
    let mut names = changes.iter().map(|c| c.name.as_str()).collect::<Vec<_>>();
    names.sort();
    let mut title = format!(
        "inputs: {} {}",
        if setting { "set" } else { "update" },
        names.join(", ")
    );
    let first = &changes[0];
    if let (Some(rev), Some(short)) = (&first.new_info.rev, &first.new_info.short_rev)
        && changes.iter().all(|c| c.new_info.rev.as_ref() == Some(rev))
        && (changes.len() == 1
            || url(first).is_some_and(|reference| {
                changes
                    .iter()
                    .all(|c| url(c).is_some_and(|u| normalized(u) == normalized(reference)))
            }))
    {
        title.push_str(&format!(" to {short}"));
    }
    title
}
fn summary(change: &InputChange) -> Vec<u8> {
    format!(
        "{}: {} -> {}\n",
        change.name,
        change
            .old_info
            .as_ref()
            .and_then(|i| i.short_rev.as_deref())
            .unwrap_or("set"),
        change.new_info.short_rev.as_deref().unwrap_or("-")
    )
    .into_bytes()
}
fn url(change: &InputChange) -> Option<&str> {
    change
        .new_info
        .url
        .as_deref()
        .or_else(|| change.old_info.as_ref().and_then(|i| i.url.as_deref()))
}
fn normalized(url: &str) -> &str {
    url.trim_end_matches('/')
}
fn groups(changes: &[InputChange]) -> Vec<Vec<&InputChange>> {
    let mut groups: Vec<Vec<&InputChange>> = Vec::new();
    for change in changes {
        let matching = url(change).and_then(|u| {
            groups.iter().position(|group| {
                let first = group[0];
                url(first).is_some_and(|reference| normalized(u) == normalized(reference))
                    && first.old_info.as_ref().and_then(|i| i.rev.as_ref())
                        == change.old_info.as_ref().and_then(|i| i.rev.as_ref())
                    && first.new_info.rev == change.new_info.rev
            })
        });
        if let Some(i) = matching {
            groups[i].push(change);
        } else {
            groups.push(vec![change]);
        }
    }
    groups
}
fn whitespace(b: &u8) -> bool {
    matches!(b, 0 | b' ' | b'\t' | b'\n' | b'\r' | 11 | 12)
}
fn trim_end(bytes: &[u8]) -> &[u8] {
    let end = bytes
        .iter()
        .rposition(|b| !whitespace(b))
        .map_or(0, |i| i + 1);
    &bytes[..end]
}
fn strip(bytes: &[u8]) -> &[u8] {
    let bytes = trim_end(bytes);
    let start = bytes
        .iter()
        .position(|b| !whitespace(b))
        .unwrap_or(bytes.len());
    &bytes[start..]
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::inputs::FlakeLock;
    use serde_json::json;
    fn change(name: &str, old: Option<&str>, new: &str, url: Option<&str>) -> InputChange {
        let info = |rev: Option<&str>| {
            FlakeLock { data: json!({"version":7,"root":"root","nodes":{"root":{"inputs":{"i":"i"}},"i":{"locked":{"rev":rev,"url":url}}}}) }.input_info("i").unwrap()
        };
        InputChange {
            name: name.into(),
            old_info: old.map(|r| info(Some(r))),
            new_info: info(Some(new)),
        }
    }
    #[test]
    fn ruby_titles_plain_summaries_group_identity_and_first_raw_url() {
        let a = change("staging", Some("1111111111"), "2222222222", Some("repo///"));
        let b = change("production", Some("1111111111"), "2222222222", Some("repo"));
        let c = change("old", Some("3333333333"), "2222222222", Some("repo/"));
        assert_eq!(
            title(&[a.clone(), b.clone()], false),
            "inputs: update production, staging to 22222222"
        );
        assert_eq!(
            title(
                &[
                    a.clone(),
                    change("other", None, "2222222222", Some("different"))
                ],
                false
            ),
            "inputs: update other, staging"
        );
        let changes = [a, b, c];
        let groups = groups(&changes);
        assert_eq!(groups.iter().map(Vec::len).collect::<Vec<_>>(), [2, 1]);
        assert_eq!(url(groups[0][0]), Some("repo///"));
        let new = change("new", None, "7777777777", None);
        assert_eq!(
            title(std::slice::from_ref(&new), true),
            "inputs: set new to 77777777"
        );
        assert_eq!(summary(&new), b"new: set -> 77777777\n");
        assert_eq!(groups_without_urls(), [1, 1]);
    }
    fn groups_without_urls() -> Vec<usize> {
        groups(&[change("a", None, "x", None), change("b", None, "x", None)])
            .iter()
            .map(Vec::len)
            .collect()
    }
    #[test]
    fn mutation_fallback_is_limited_to_cache_availability() {
        for e in [
            ConfctlError::Spawn {
                description: "git".into(),
                source: std::io::Error::other("spawn"),
            },
            ConfctlError::Drain {
                stream: "stdout",
                source: std::io::Error::other("drain"),
            },
            ConfctlError::Cancelled,
            ConfctlError::EventDelivery("closed".into()),
            ConfctlError::State("capture incomplete".into()),
            ConfctlError::Io {
                operation: "read captured output",
                path: None,
                source: std::io::Error::other("read"),
            },
        ] {
            assert!(!cache_failure(&e));
        }
    }
}
