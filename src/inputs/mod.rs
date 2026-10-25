//! Flake input information, logical lock graphs and guarded lock editing.

mod editor;
mod forward;
mod graph;
mod lock;
pub use editor::*;
pub use forward::*;
pub use graph::*;
pub use lock::*;

/// A successful Nix command can still have used stale GitHub metadata. Such a
/// result must fail before setter publication or an updater commit.
pub fn check_nix_output(stdout: &str, stderr: &str) -> Result<(), crate::error::ConfctlError> {
    let output = format!("{stdout}\n{stderr}");
    let fallback = output
        .find("unable to download 'https://api.github.com/")
        .is_some_and(|start| output[start..].contains("using cached version"));
    if !fallback {
        return Ok(());
    }
    Err(crate::error::ConfctlError::Configuration(
        if output.to_lowercase().contains("rate limit exceeded") {
            "Nix could not refresh GitHub input metadata because the GitHub API rate limit was exceeded.\nIt used a cached version instead, so the selected input may not be the latest revision.\nConfigure a GitHub token in Nix access-tokens or retry later."
        } else {
            "Nix could not refresh GitHub input metadata and used a cached version instead.\nThe selected input may not be the latest revision; retry after the GitHub API is reachable."
        }.into(),
    ))
}

#[cfg(test)]
mod tests;
