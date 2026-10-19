use serde_json::{Value, json};
use std::{
    io::{self, Read, Write},
    os::unix::fs::symlink,
    path::{Path, PathBuf},
    process::{Command, ExitStatus},
};

struct MetadataJsonFailure {
    path: PathBuf,
    source: serde_json::Error,
}

impl std::fmt::Debug for MetadataJsonFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("MetadataJsonFailure")
            .field("category", &self.source.classify())
            .finish_non_exhaustive()
    }
}

impl std::fmt::Display for MetadataJsonFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Invalid machine metadata JSON: {}", self.path.display())
    }
}

impl std::error::Error for MetadataJsonFailure {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(&self.source)
    }
}

struct CurrentProfileMissing {
    path: PathBuf,
    source: io::Error,
}

impl std::fmt::Debug for CurrentProfileMissing {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CurrentProfileMissing")
            .field("kind", &self.source.kind())
            .finish_non_exhaustive()
    }
}

impl std::fmt::Display for CurrentProfileMissing {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "No such file or directory: {}", self.path.display())
    }
}

impl std::error::Error for CurrentProfileMissing {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(&self.source)
    }
}

struct InstallFailure {
    target: PathBuf,
    status: ExitStatus,
}

impl std::fmt::Debug for InstallFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("InstallFailure")
            .field("status", &self.status)
            .finish_non_exhaustive()
    }
}

impl std::fmt::Display for InstallFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Failed to install new root to {}", self.target.display())
    }
}

impl std::error::Error for InstallFailure {}

struct Memtest {
    package: PathBuf,
    params: Vec<String>,
}
struct IsoImage {
    file: PathBuf,
    label: String,
    name: String,
}
struct NetbootServerConfig {
    coreutils: PathBuf,
    syslinux: PathBuf,
    tftp_root: PathBuf,
    http_root: PathBuf,
    host_name: String,
    http_url: String,
    memtest: Option<Memtest>,
    iso_images: Vec<IsoImage>,
}

fn invalid(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.into())
}
fn string(value: &Value, key: &str) -> io::Result<String> {
    value
        .get(key)
        .and_then(Value::as_str)
        .map(str::to_owned)
        .ok_or_else(|| invalid(format!("missing or invalid {key}")))
}
fn strings(value: &Value, key: &str) -> io::Result<Vec<String>> {
    value
        .get(key)
        .and_then(Value::as_array)
        .ok_or_else(|| invalid(format!("missing or invalid {key}")))?
        .iter()
        .map(|value| {
            value
                .as_str()
                .map(str::to_owned)
                .ok_or_else(|| invalid(format!("invalid {key} element")))
        })
        .collect()
}

impl NetbootServerConfig {
    fn load(path: &Path) -> io::Result<Self> {
        let value: Value =
            serde_json::from_slice(&std::fs::read(path)?).map_err(io::Error::other)?;
        let memtest = match value
            .get("memtest")
            .ok_or_else(|| invalid("missing memtest"))?
        {
            Value::Null => None,
            value => Some(Memtest {
                package: string(value, "package")?.into(),
                params: strings(value, "params")?,
            }),
        };
        let iso_images = value
            .get("isoImages")
            .and_then(Value::as_array)
            .ok_or_else(|| invalid("missing or invalid isoImages"))?
            .iter()
            .enumerate()
            .map(|(index, value)| {
                let file: PathBuf = string(value, "file")?.into();
                let mut label = string(value, "label")?;
                let basename = file
                    .file_name()
                    .and_then(|name| name.to_str())
                    .ok_or_else(|| invalid("invalid ISO basename"))?;
                if label.is_empty() {
                    label = basename.into();
                }
                let name = format!("{index:02}-{basename}");
                Ok(IsoImage { file, label, name })
            })
            .collect::<io::Result<Vec<_>>>()?;
        // Old JSON's unused ruby key is intentionally accepted and ignored.
        Ok(Self {
            coreutils: string(&value, "coreutils")?.into(),
            syslinux: string(&value, "syslinux")?.into(),
            tftp_root: string(&value, "tftpRoot")?.into(),
            http_root: string(&value, "httpRoot")?.into(),
            host_name: string(&value, "hostName")?,
            http_url: string(&value, "httpUrl")?,
            memtest,
            iso_images,
        })
    }
}

fn joined(parent: &str, child: &str) -> String {
    format!(
        "{}/{}",
        parent.trim_end_matches('/'),
        child.trim_start_matches('/')
    )
}
fn at(root: &Path, relative: &str) -> PathBuf {
    root.join(relative.trim_start_matches('/'))
}
fn write(root: &Path, relative: &str, bytes: &[u8]) -> io::Result<()> {
    let path = at(root, relative);
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    std::fs::write(path, bytes)
}

struct Root {
    target: PathBuf,
    new: PathBuf,
    old: Option<PathBuf>,
}

impl Root {
    fn new(target: &Path, suffix: &str) -> io::Result<Self> {
        let old = match std::fs::read_link(target) {
            Ok(path) => Some(if path.is_absolute() {
                path
            } else {
                target.parent().unwrap_or(Path::new(".")).join(path)
            }),
            Err(error) if error.kind() == io::ErrorKind::NotFound => None,
            Err(error) => return Err(error),
        };
        let mut new = target.as_os_str().to_owned();
        new.push(format!(".{suffix}"));
        Ok(Self {
            target: target.into(),
            new: new.into(),
            old,
        })
    }
    fn install(&self, config: &NetbootServerConfig) -> io::Result<()> {
        #[cfg(test)]
        super::helper_diagnostic_receipts::record(super::helper_diagnostic_receipts::Event::InstallEntered { target: self.target.clone(), new: self.new.clone() });
        let mut command = Command::new(config.coreutils.join("bin/ln"));
        command
            .arg("-sfn")
            .arg(
                self.new
                    .file_name()
                    .ok_or_else(|| invalid("invalid new root basename"))?,
            )
            .arg(&self.target);
        #[cfg(test)]
        super::helper_diagnostic_receipts::command_start(&command);
        let status = command.status()?;
        #[cfg(test)]
        super::helper_diagnostic_receipts::command_wait(&command, &status);
        if status.success() {
            Ok(())
        } else {
            Err(io::Error::other(InstallFailure {
                target: self.target.clone(),
                status,
            }))
        }
    }
    fn cleanup(&self) -> io::Result<()> {
        #[cfg(test)]
        super::helper_diagnostic_receipts::record(super::helper_diagnostic_receipts::Event::CleanupEntered { target: self.target.clone(), old: self.old.clone() });
        if let Some(path) = &self.old {
            if std::fs::symlink_metadata(path)?.is_dir() {
                std::fs::remove_dir_all(path)?;
            } else {
                std::fs::remove_file(path)?;
            }
        }
        Ok(())
    }
}

fn tftp(root: &Path, config: &NetbootServerConfig, machines: &[model::Machine]) -> io::Result<()> {
    std::fs::create_dir_all(root)?;
    for program in [
        "pxelinux.0",
        "ldlinux.c32",
        "libcom32.c32",
        "libutil.c32",
        "memdisk",
        "menu.c32",
        "reboot.c32",
    ] {
        std::fs::copy(
            config.syslinux.join("share/syslinux").join(program),
            root.join(program),
        )?;
    }
    std::fs::create_dir_all(root.join("pxelinux.cfg"))?;
    std::fs::create_dir_all(root.join("pxeserver"))?;
    for machine in machines {
        for generation in &machine.generations {
            let relative = format!("boot/{}/{}", machine.fqdn, generation.number);
            std::fs::create_dir_all(at(root, &relative))?;
            for file in generation
                .boot_files
                .iter()
                .filter(|file| matches!(file.name.as_str(), "bzImage" | "initrd"))
            {
                symlink(&file.path, at(root, &format!("{relative}/{}", file.name)))?;
            }
            if generation.current {
                symlink(
                    &generation.number,
                    at(root, &format!("boot/{}/current", machine.fqdn)),
                )?;
            }
        }
        for mac in &machine.current().macs {
            symlink(
                format!("../pxeserver/machines/{}/auto.cfg", machine.fqdn),
                at(root, &format!("pxelinux.cfg/01-{}", mac.replace(':', "-"))),
            )?;
        }
    }
    if let Some(memtest) = &config.memtest {
        std::fs::create_dir_all(root.join("boot/memtest86"))?;
        symlink(
            memtest.package.join("memtest.bin"),
            root.join("boot/memtest86/memtest.bin"),
        )?;
    }
    if !config.iso_images.is_empty() {
        std::fs::create_dir_all(root.join("boot/iso-images"))?;
        for image in &config.iso_images {
            symlink(
                &image.file,
                at(root, &format!("boot/iso-images/{}", image.name)),
            )?;
        }
    }
    render::tftp(root, config, machines)
}

fn pretty(value: &Value) -> io::Result<Vec<u8>> {
    super::json::pretty(value).map_err(io::Error::other)
}
fn http(root: &Path, machines: &[model::Machine]) -> io::Result<()> {
    std::fs::create_dir_all(root)?;
    for machine in machines {
        for generation in &machine.generations {
            let relative = format!("{}/{}", machine.fqdn, generation.number);
            std::fs::create_dir_all(at(root, &relative))?;
            for file in &generation.boot_files {
                symlink(&file.path, at(root, &format!("{relative}/{}", file.name)))?;
            }
            write(
                root,
                &format!("{relative}/generation.json"),
                &pretty(&generation.json())?,
            )?;
            write(
                root,
                &format!("{relative}/kernel-params"),
                generation.params.join(" ").as_bytes(),
            )?;
            if generation.current {
                symlink(
                    &generation.number,
                    at(root, &format!("{}/current", machine.fqdn)),
                )?;
            }
        }
        write(
            root,
            &format!("{}/machine.json", machine.fqdn),
            &pretty(&machine.json())?,
        )?;
    }
    write(
        root,
        "machines.json",
        &pretty(
            &json!({"machines":machines.iter().map(model::Machine::json).collect::<Vec<_>>()}),
        )?,
    )
}

fn publish(
    config: &NetbootServerConfig,
    machines: &[model::Machine],
    suffix: &str,
) -> io::Result<()> {
    #[cfg(test)]
    super::helper_diagnostic_receipts::record(super::helper_diagnostic_receipts::Event::PublishEntered { tftp: config.tftp_root.clone(), http: config.http_root.clone(), suffix: suffix.into() });
    // Construct both before running either, matching the old readlink boundary.
    let tftp_root = Root::new(&config.tftp_root, suffix)?;
    let http_root = Root::new(&config.http_root, suffix)?;
    tftp(&tftp_root.new, config, machines)?;
    http(&http_root.new, machines)?;
    tftp_root.install(config)?;
    http_root.install(config)?;
    tftp_root.cleanup()?;
    http_root.cleanup()
}

fn prepare(
    config: &NetbootServerConfig,
    profiles: &Path,
    lock: &Path,
    out: &mut impl Write,
) -> io::Result<(std::fs::File, Vec<model::Machine>)> {
    std::fs::create_dir_all(
        lock.parent()
            .ok_or_else(|| invalid("invalid netboot lock path"))?,
    )?;
    #[cfg(test)]
    super::helper_diagnostic_receipts::record(super::helper_diagnostic_receipts::Event::LockOpen { path: lock.into() });
    let lock = std::fs::OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .open(lock)?;
    rustix::fs::flock(&lock, rustix::fs::FlockOperation::LockExclusive)?;
    #[cfg(test)]
    super::helper_diagnostic_receipts::lock_acquired(&lock);
    let machines = model::load(config, profiles)?;
    for machine in &machines {
        writeln!(out, "{}", machine.fqdn)?;
        for generation in &machine.generations {
            writeln!(
                out,
                "  - {} - {}{}",
                generation.time_s,
                generation.version.as_deref().unwrap_or(""),
                if generation.current { " (current)" } else { "" }
            )?;
        }
        writeln!(out)?;
    }
    #[cfg(test)]
    super::helper_diagnostic_receipts::record(super::helper_diagnostic_receipts::Event::PrepareCompleted { machines: machines.len() });
    Ok((lock, machines))
}

fn run(
    config: &NetbootServerConfig,
    profiles: &Path,
    lock: &Path,
    entropy: &Path,
    out: &mut impl Write,
) -> io::Result<()> {
    let (_lock, machines) = prepare(config, profiles, lock, out)?;
    let mut random = [0u8; 3];
    #[cfg(test)]
    super::helper_diagnostic_receipts::record(super::helper_diagnostic_receipts::Event::EntropyOpen { path: entropy.into() });
    std::fs::File::open(entropy)?.read_exact(&mut random)?;
    #[cfg(test)]
    super::helper_diagnostic_receipts::record(super::helper_diagnostic_receipts::Event::EntropyRead { bytes: random });
    let suffix = random
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect::<String>();
    publish(config, &machines, &suffix)
}

struct BuilderPaths<'a> {
    profiles: &'a Path,
    lock: &'a Path,
    entropy: &'a Path,
}

fn common_entry(paths: &BuilderPaths<'_>, stdout: &mut impl Write, stderr: &mut impl Write) -> i32 {
    // Original builder has no public parser; surplus argv remains unused.
    let result = (|| -> io::Result<()> {
        let path = std::env::var_os("CONFCTL_INTERNAL_NETBOOT_CONFIG")
            .map(PathBuf::from)
            .filter(|path| path.is_absolute())
            .ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "missing or invalid package field CONFCTL_INTERNAL_NETBOOT_CONFIG",
                )
            })?;
        run(
            &NetbootServerConfig::load(&path)?,
            paths.profiles,
            paths.lock,
            paths.entropy,
            stdout,
        )
    })();
    finish(result, stderr)
}


fn finish(result: io::Result<()>, stderr: &mut impl Write) -> i32 {
    match result {
        Ok(()) => 0,
        Err(error) => {
            let _ = writeln!(stderr, "{error}");
            1
        }
    }
}
