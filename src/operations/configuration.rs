//! Core configuration edits followed by the accepted post-write netboot stage.
//! The one advisory guard spans local effects and every joined Nix child.

use crate::{
    app::Context,
    configuration::{ConfigEpoch, ConfigRoot, MutationGuard, MutationLock, StateFiles},
    domain::Action,
    error::ConfctlError,
    events::{EventSink, OutputStream, TaskId, UiEvent},
    netboot_inventory::{self, FeatureFiles},
    nix::{InvocationSettings, NixClient, NixOptions},
};
use std::{
    fs,
    io::{self, Write},
    os::unix::{
        ffi::OsStrExt,
        fs::{DirBuilderExt, OpenOptionsExt},
    },
    path::{Path, PathBuf},
};
use tokio_util::sync::CancellationToken;

pub(super) async fn run(
    action: &Action,
    root: ConfigRoot,
    ctx: &Context,
) -> Result<(), ConfctlError> {
    preflight(action, &root)?;
    let guard = MutationLock::acquire(&root, &ctx.cancellation).await?;
    preflight(action, &root)?;
    core(action, &root, &guard, &ctx.events, &ctx.cancellation).await.map_err(|error| {
        error.with_secondary(vec![ConfctlError::State(
            "configuration edit stopped; completed local effects are retained; inspect cluster before retrying".into(),
        )])
    })?;
    // No client or settings snapshot exists before this publication boundary.
    let mut epoch = ConfigEpoch::default();
    epoch.0 += 1;
    feature(&root, &guard, ctx, epoch).await.map_err(|error| {
        error.with_secondary(vec![ConfctlError::State(
            "core cluster list replaced; later netboot stage failed; inspect the configuration and explicitly rediscover".into(),
        )])
    })
}

fn preflight(action: &Action, root: &ConfigRoot) -> Result<(), ConfctlError> {
    root.require_flake()?;
    match action {
        Action::Add { name } => {
            let destination = named(root, name)?;
            absent(&destination)?;
        }
        Action::Rename { old, new } => {
            let source = named(root, old)?;
            let destination = named(root, new)?;
            let resolved = source
                .canonicalize()
                .map_err(|e| io_error("resolve rename source", &source, e))?;
            if !resolved.is_dir() || !resolved.starts_with(cluster(root)?) {
                return Err(ConfctlError::Configuration(format!(
                    "{} is not a directory within cluster",
                    source.display()
                )));
            }
            absent(&destination)?;
            if !fs::symlink_metadata(&source)
                .map_err(|e| io_error("inspect rename source", &source, e))?
                .file_type()
                .is_symlink()
                && (destination.starts_with(&source)
                    || nearest_parent(&destination)?.starts_with(&resolved))
            {
                return Err(ConfctlError::Configuration(
                    "rename destination is inside its source directory".into(),
                ));
            }
        }
        Action::Rediscover => {
            cluster(root)?;
        }
        _ => unreachable!("configuration adapter receives add/rename/rediscover"),
    }
    legacy_scripts(root)
}

fn legacy_scripts(root: &ConfigRoot) -> Result<(), ConfctlError> {
    let path = root.path().join("scripts");
    let entries = match fs::read_dir(&path) {
        Ok(entries) => entries,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(io_error("inspect legacy scripts", &path, e)),
    };
    for entry in entries {
        let entry = entry.map_err(|e| io_error("inspect legacy script entry", &path, e))?;
        match fs::metadata(entry.path()) {
            Ok(metadata) if metadata.is_file() => {
                return Err(ConfctlError::Configuration(format!(
                    "legacy user script {} is not supported; migrate its hook to the matching confctl module/settings and remove the script before editing configuration",
                    entry.path().display()
                )));
            }
            Ok(_) => {}
            Err(e) if e.kind() == io::ErrorKind::NotFound => {}
            Err(e) => return Err(io_error("inspect legacy script", &entry.path(), e)),
        }
    }
    Ok(())
}

// Validate raw components: Path::components would silently erase interior '.'.
fn named(root: &ConfigRoot, name: &Path) -> Result<PathBuf, ConfctlError> {
    if name.is_absolute()
        || name
            .as_os_str()
            .as_bytes()
            .split(|b| *b == b'/')
            .any(|part| part.is_empty() || part == b"." || part == b"..")
    {
        return Err(ConfctlError::Configuration(
            "machine name must contain nonempty relative components without . or ..".into(),
        ));
    }
    let path = root.path().join("cluster").join(name);
    within_parent(root, &path)?;
    Ok(path)
}

fn cluster(root: &ConfigRoot) -> Result<PathBuf, ConfctlError> {
    let path = root.path().join("cluster");
    let resolved = match fs::symlink_metadata(&path) {
        Ok(_) => path
            .canonicalize()
            .map_err(|e| io_error("resolve cluster directory", &path, e))?,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(path),
        Err(e) => return Err(io_error("inspect cluster directory", &path, e)),
    };
    if !resolved.is_dir() || !resolved.starts_with(&path) {
        return Err(ConfctlError::Configuration(
            "cluster must resolve within the configuration's cluster tree".into(),
        ));
    }
    Ok(resolved)
}

fn nearest_parent(path: &Path) -> Result<PathBuf, ConfctlError> {
    let mut parent = path.parent().expect("literal cluster child");
    loop {
        match fs::symlink_metadata(parent) {
            Ok(_) => {
                let resolved = parent
                    .canonicalize()
                    .map_err(|e| io_error("resolve configuration parent", parent, e))?;
                if !resolved.is_dir() {
                    return Err(ConfctlError::Configuration(format!(
                        "{} is not a directory",
                        parent.display()
                    )));
                }
                return Ok(resolved);
            }
            Err(e) if e.kind() == io::ErrorKind::NotFound => {
                parent = parent.parent().expect("canonical root exists")
            }
            Err(e) => return Err(io_error("inspect configuration parent", parent, e)),
        }
    }
}

fn within_parent(root: &ConfigRoot, path: &Path) -> Result<(), ConfctlError> {
    let boundary = cluster(root)?;
    let parent = nearest_parent(path)?;
    if !(parent.starts_with(&boundary)
        || boundary == root.path().join("cluster") && parent == root.path() && !boundary.exists())
    {
        return Err(ConfctlError::Configuration(format!(
            "{} resolves outside cluster",
            path.display()
        )));
    }
    Ok(())
}
fn absent(path: &Path) -> Result<(), ConfctlError> {
    match fs::symlink_metadata(path) {
        Ok(_) => Err(ConfctlError::Configuration(format!(
            "{} already exists",
            path.display()
        ))),
        Err(e) if e.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(io_error("inspect configuration destination", path, e)),
    }
}
fn canceled(cancellation: &CancellationToken) -> Result<(), ConfctlError> {
    if cancellation.is_cancelled() {
        Err(ConfctlError::Cancelled)
    } else {
        Ok(())
    }
}
async fn notice(
    events: &EventSink,
    cancellation: &CancellationToken,
    verb: &[u8],
    paths: &[&Path],
) -> Result<(), ConfctlError> {
    canceled(cancellation)?;
    let mut bytes = verb.to_vec();
    for path in paths {
        bytes.push(b' ');
        bytes.extend_from_slice(path.as_os_str().as_bytes());
    }
    bytes.push(b'\n');
    events
        .emit(UiEvent::Output {
            task: TaskId(0),
            stream: OutputStream::Stdout,
            bytes,
        })
        .await?;
    canceled(cancellation)
}
fn mkdir(
    root: &ConfigRoot,
    guard: &MutationGuard,
    relative: &Path,
    exclusive: bool,
) -> Result<(), ConfctlError> {
    guard.require_root(root)?;
    let path = root.path().join(relative);
    if relative != Path::new("cluster") {
        within_parent(root, &path)?;
    }
    let mut builder = fs::DirBuilder::new();
    builder.recursive(true).mode(0o755);
    if exclusive {
        absent(&path)?;
        builder
            .create(path.parent().expect("literal child"))
            .map_err(|e| io_error("create configuration parents", &path, e))?;
        fs::DirBuilder::new().mode(0o755).create(&path)
    } else {
        builder.create(&path)
    }
    .map_err(|e| io_error("create configuration directory", &path, e))
}
fn mkfile(
    root: &ConfigRoot,
    guard: &MutationGuard,
    relative: &Path,
    bytes: &[u8],
) -> Result<(), ConfctlError> {
    guard.require_root(root)?;
    let path = root.path().join(relative);
    within_parent(root, &path)?;
    fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o644)
        .open(&path)
        .and_then(|mut file| file.write_all(bytes))
        .map_err(|e| io_error("write configuration template", &path, e))
}

async fn core(
    action: &Action,
    root: &ConfigRoot,
    guard: &MutationGuard,
    events: &EventSink,
    cancellation: &CancellationToken,
) -> Result<(), ConfctlError> {
    match action {
        Action::Add { name } => {
            let directory = Path::new("cluster").join(name);
            notice(events, cancellation, b"mkdir", &[&directory]).await?;
            mkdir(root, guard, &directory, true)?;
            let (module, config) = templates(name);
            for (file, bytes) in [("module.nix", module), ("config.nix", config)] {
                let path = directory.join(file);
                notice(events, cancellation, b"mkfile", &[&path]).await?;
                mkfile(root, guard, &path, &bytes)?;
            }
        }
        Action::Rename { old, new } => {
            let source = Path::new("cluster").join(old);
            let destination = Path::new("cluster").join(new);
            let parent = destination.parent().expect("literal cluster child");
            notice(events, cancellation, b"mkdir", &[parent]).await?;
            mkdir(root, guard, parent, false)?;
            notice(events, cancellation, b"mv", &[&source, &destination]).await?;
            guard.require_root(root)?;
            preflight(action, root)?;
            rustix::fs::renameat_with(
                rustix::fs::CWD,
                root.path().join(source),
                rustix::fs::CWD,
                root.path().join(destination),
                rustix::fs::RenameFlags::NOREPLACE,
            )
            .map_err(|e| io_error("rename configuration directory", new, e.into()))?;
        }
        Action::Rediscover => {}
        _ => unreachable!(),
    }
    let bytes = discovered(root, cancellation)?;
    let path = Path::new("cluster/cluster.nix");
    notice(events, cancellation, b"replace", &[path]).await?;
    replace_core(root, guard, &bytes)?;
    canceled(cancellation)
}

fn templates(name: &Path) -> (Vec<u8>, Vec<u8>) {
    let name = name.as_os_str().as_bytes();
    let module = [b"{ config, ... }:\n{\n  cluster.\"".as_slice(), name, b"\" = {\n    spin = \"nixos\";\n    inputs.channels = [ \"nixos-unstable\" ];\n    host = { name = \"machine\"; domain = \"example.com\"; };\n  };\n}\n"].concat();
    let depth = name.iter().filter(|b| **b == b'/').count() + 2;
    let hostname = name
        .iter()
        .map(|b| if *b == b'/' { b'-' } else { *b })
        .collect::<Vec<_>>();
    let config = [b"{ config, pkgs, lib, ... }:\n{\n  imports = [\n    ".as_slice(), b"../".repeat(depth).as_slice(), b"environments/base.nix\n  ];\n\n  # ... standard NixOS configuration ...\n\n  networking.hostName = \"", &hostname, b"\";\n}\n"].concat();
    (module, config)
}

fn discovered(
    root: &ConfigRoot,
    cancellation: &CancellationToken,
) -> Result<Vec<u8>, ConfctlError> {
    let boundary = cluster(root)?;
    let mut names = Vec::new();
    discover(
        &root.path().join("cluster"),
        Path::new(""),
        &boundary,
        &mut vec![boundary.clone()],
        &mut names,
        cancellation,
    )?;
    names.sort_by(|left, right| {
        left.as_os_str()
            .as_bytes()
            .cmp(right.as_os_str().as_bytes())
    });
    let mut bytes = b"# This file is generated by confctl, changes will be lost\n[\n".to_vec();
    for name in names {
        bytes.extend_from_slice(b"  ./");
        bytes.extend_from_slice(name.as_os_str().as_bytes());
        bytes.extend_from_slice(b"/module.nix\n");
    }
    bytes.extend_from_slice(b"]\n");
    Ok(bytes)
}
fn discover(
    directory: &Path,
    relative: &Path,
    boundary: &Path,
    ancestors: &mut Vec<PathBuf>,
    names: &mut Vec<PathBuf>,
    cancellation: &CancellationToken,
) -> Result<(), ConfctlError> {
    canceled(cancellation)?;
    for entry in
        fs::read_dir(directory).map_err(|e| io_error("discover cluster directory", directory, e))?
    {
        canceled(cancellation)?;
        let entry = entry.map_err(|e| io_error("discover cluster entry", directory, e))?;
        let path = entry.path();
        let metadata = match fs::metadata(&path) {
            Ok(metadata) => metadata,
            Err(e) if e.kind() == io::ErrorKind::NotFound => continue,
            Err(e) => return Err(io_error("inspect cluster entry", &path, e)),
        };
        if !metadata.is_dir() {
            continue;
        }
        let canonical = path
            .canonicalize()
            .map_err(|e| io_error("resolve discovered directory", &path, e))?;
        if !canonical.starts_with(boundary) || ancestors.contains(&canonical) {
            return Err(ConfctlError::Configuration(format!(
                "{} is an escaping or cyclic cluster directory link",
                path.display()
            )));
        }
        let name = relative.join(entry.file_name());
        if path
            .join("module.nix")
            .try_exists()
            .map_err(|e| io_error("inspect discovered module", &path, e))?
            && path
                .join("config.nix")
                .try_exists()
                .map_err(|e| io_error("inspect discovered config", &path, e))?
        {
            names.push(name.clone());
        }
        ancestors.push(canonical);
        discover(&path, &name, boundary, ancestors, names, cancellation)?;
        ancestors.pop();
    }
    Ok(())
}
fn replace_core(
    root: &ConfigRoot,
    guard: &MutationGuard,
    bytes: &[u8],
) -> Result<(), ConfctlError> {
    guard.require_root(root)?;
    let directory = cluster(root)?;
    let path = directory.join("cluster.nix");
    let temporary = tempfile::Builder::new()
        .prefix("cluster.nix.new-")
        .permissions(fs::Permissions::from_mode(0o644))
        .tempfile_in(&directory)
        .map_err(|e| io_error("create core list sibling", &path, e))?;
    // Keep a partially written sibling on failure, as Ruby's replace_file does.
    let (mut file, staging) = temporary
        .keep()
        .map_err(|e| io_error("retain core list sibling", &path, e.error))?;
    file.write_all(bytes)
        .and_then(|()| file.sync_all())
        .map_err(|e| io_error("write core list sibling", &staging, e))?;
    fs::rename(&staging, &path).map_err(|e| io_error("publish core cluster list", &path, e))
}

async fn feature(
    root: &ConfigRoot,
    guard: &MutationGuard,
    ctx: &Context,
    epoch: ConfigEpoch,
) -> Result<(), ConfctlError> {
    canceled(&ctx.cancellation)?;
    let defaults = InvocationSettings::new(
        root.clone(),
        ctx.processes.clone(),
        ctx.cancellation.clone(),
    );
    let bootstrap = defaults.read().await?;
    canceled(&ctx.cancellation)?;
    if !bootstrap.netboot_inventory.enable {
        return Ok(());
    }
    let mut nix = NixClient::configured(
        root.clone(),
        ctx.processes.clone(),
        NixOptions::default(),
        ctx.cancellation.clone(),
        &defaults,
        epoch,
    )
    .await?;
    canceled(&ctx.cancellation)?;
    let inventory = nix.inventory().await?;
    let bytes = netboot_inventory::render(inventory.require_epoch(epoch)?)?;
    canceled(&ctx.cancellation)?;
    let path = bootstrap
        .netboot_inventory
        .output_file
        .as_ref()
        .expect("enabled settings validated output path");
    notice(
        &ctx.events,
        &ctx.cancellation,
        b"replace",
        &[path.as_path()],
    )
    .await?;
    netboot_inventory::publish(
        &FeatureFiles {
            files: &StateFiles::new(root.clone()),
            settings: &bootstrap.netboot_inventory,
            mutation: guard,
        },
        &bytes,
    )?;
    drop(inventory);
    nix.invalidate_configuration();
    canceled(&ctx.cancellation)
}
fn io_error(operation: &'static str, path: &Path, source: io::Error) -> ConfctlError {
    ConfctlError::Io {
        operation,
        path: Some(path.into()),
        source,
    }
    .with_secondary(vec![ConfctlError::State(format!(
        "configuration path: {}",
        path.display()
    ))])
}
use std::os::unix::fs::PermissionsExt;

#[cfg(test)]
mod tests;
