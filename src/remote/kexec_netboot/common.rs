use reqwest::{Client, Url, header::ACCEPT_ENCODING};
use serde_json::Value;
use std::{
    ffi::{OsStr, OsString},
    io::{self, BufRead, Write},
    path::{Path, PathBuf},
    process::Command,
    time::Duration,
};
use tokio::io::AsyncReadExt;

const ACCEPTED_ENCODING: &str = "gzip;q=1.0,deflate;q=0.6,identity;q=0.3";

struct IndexJsonFailure {
    source: serde_json::Error,
}

struct TransferFailure {
    primary: io::Error,
    cleanup: Vec<(PathBuf, io::Error)>,
}

impl std::fmt::Debug for TransferFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TransferFailure")
            .field("primary_kind", &self.primary.kind())
            .field(
                "cleanup_kinds",
                &self
                    .cleanup
                    .iter()
                    .map(|(_, error)| error.kind())
                    .collect::<Vec<_>>(),
            )
            .finish_non_exhaustive()
    }
}

impl std::fmt::Display for TransferFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.primary)?;
        for (path, error) in &self.cleanup {
            write!(
                f,
                "; cleanup: Could not remove tmp file {}: {error}",
                path.display()
            )?;
        }
        Ok(())
    }
}

impl std::error::Error for TransferFailure {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(&self.primary)
    }
}

fn cleanup_failed(primary: io::Error, files: Vec<tempfile::TempPath>) -> io::Error {
    let cleanup = files
        .into_iter()
        .filter_map(|file| {
            let path = file.to_path_buf();
            file.close().err().map(|error| (path, error))
        })
        .collect();
    io::Error::new(primary.kind(), TransferFailure { primary, cleanup })
}

impl std::fmt::Debug for IndexJsonFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("IndexJsonFailure")
            .field("category", &self.source.classify())
            .field("line", &self.source.line())
            .field("column", &self.source.column())
            .finish_non_exhaustive()
    }
}

impl std::fmt::Display for IndexJsonFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "ERROR: Could not parse JSON: invalid JSON at line {} column {}",
            self.source.line(),
            self.source.column()
        )
    }
}

impl std::error::Error for IndexJsonFailure {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        Some(&self.source)
    }
}

struct KexecNetbootConfig {
    default_fqdn: String,
    kexec: PathBuf,
}

impl KexecNetbootConfig {
    fn from_package() -> io::Result<Self> {
        let default_fqdn = std::env::var("CONFCTL_INTERNAL_KEXEC_MACHINE_FQDN").map_err(|_| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                "missing or invalid package field CONFCTL_INTERNAL_KEXEC_MACHINE_FQDN",
            )
        })?;
        let kexec = std::env::var_os("CONFCTL_INTERNAL_KEXEC_EXECUTABLE")
            .map(PathBuf::from)
            .filter(|path| path.is_absolute())
            .ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::InvalidInput,
                    "missing or invalid package field CONFCTL_INTERNAL_KEXEC_EXECUTABLE",
                )
            })?;
        Ok(Self {
            default_fqdn,
            kexec,
        })
    }
}

#[derive(Default)]
struct Options {
    server_url: Option<String>,
    machine: Option<String>,
    generation: Option<String>,
    variant: Option<String>,
    interactive: bool,
    append: String,
    exec: bool,
    unload: bool,
}

enum Parsed {
    Run(Options),
    Help,
}

fn parse(args: &[OsString]) -> io::Result<Parsed> {
    let mut options = Options::default();
    let mut args = args.iter();
    while let Some(argument) = args.next() {
        let text = argument.to_str().ok_or_else(|| {
            io::Error::new(io::ErrorKind::InvalidInput, "invalid argument encoding")
        })?;
        if text == "--" {
            break;
        }
        if !text.starts_with('-') || text == "-" {
            continue;
        } // OptionParser leaves surplus args unused.
        if matches!(text, "-h" | "--help") {
            return Ok(Parsed::Help);
        }
        let switches = if text.starts_with("--") {
            let (flag, attached) = text
                .split_once('=')
                .map_or((text, None), |(a, b)| (a, Some(b)));
            let flags = [
                ("--server-url", 's'),
                ("--machine", 'm'),
                ("--generation", 'g'),
                ("--variant", 'v'),
                ("--interactive", 'i'),
                ("--append", 'a'),
                ("--exec", 'e'),
                ("--unload", 'u'),
                ("--no-sync", 'y'),
            ];
            let mut matching = flags.iter().filter(|(long, _)| long.starts_with(flag));
            let letter = match (matching.next(), matching.next()) {
                (Some((_, letter)), None) => *letter,
                _ => {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidInput,
                        format!("invalid or ambiguous option: {flag}"),
                    ));
                }
            };
            if attached.is_some() && matches!(letter, 'i' | 'e' | 'u' | 'y') {
                return Err(io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!("option takes no argument: {flag}"),
                ));
            }
            vec![(letter, attached)]
        } else {
            let mut switches = Vec::new();
            for (offset, letter) in text[1..].char_indices() {
                if matches!(letter, 's' | 'm' | 'g' | 'v' | 'a') {
                    let rest = &text[1 + offset + letter.len_utf8()..];
                    switches.push((
                        letter,
                        (!rest.is_empty()).then(|| rest.strip_prefix('=').unwrap_or(rest)),
                    ));
                    break; // An argument-taking option owns the rest of its cluster.
                }
                switches.push((letter, None));
            }
            switches
        };
        for (letter, attached) in switches {
            match letter {
                'i' => options.interactive = true,
                'e' => options.exec = true,
                'u' => options.unload = true,
                'y' => {} // Original @sync starts false and is never enabled.
                's' | 'm' | 'g' | 'v' | 'a' => {
                    let value = if let Some(value) = attached {
                        value.to_owned()
                    } else {
                        args.next()
                            .and_then(|value| value.to_str())
                            .ok_or_else(|| {
                                io::Error::new(
                                    io::ErrorKind::InvalidInput,
                                    format!("missing argument: -{letter}"),
                                )
                            })?
                            .to_owned()
                    };
                    match letter {
                        's' => options.server_url = Some(value),
                        'm' => options.machine = Some(value),
                        'g' => options.generation = Some(value),
                        'v' => options.variant = Some(value),
                        'a' => options.append = value,
                        _ => unreachable!(),
                    }
                }
                _ => {
                    return Err(io::Error::new(
                        io::ErrorKind::InvalidInput,
                        format!("invalid option: -{letter}"),
                    ));
                }
            }
        }
    }
    Ok(Parsed::Run(options))
}

fn help(program: &OsStr) -> String {
    format!(
        "Usage: {} [options]\n    -s, --server-url URL             Specify URL of the netboot server\n    -m, --machine FQDN               Select machine by FQDN\n    -g, --generation N               Select generation by number or negative offset\n    -v, --variant NAME               Select a specific variant by name\n    -i, --interactive                Enable interactive mode\n    -a, --append PARAMS              Append parameters to kernel command line\n    -e, --exec                       Run the currently loaded kernel\n    -u, --unload                     Unload the current kexec target kernel and exit\n    -y, --no-sync                    Skip syncing filesystems\n",
        program.to_string_lossy()
    )
}

fn text(value: &Value) -> String {
    match value {
        Value::Null => String::new(),
        Value::String(text) => text.clone(),
        value => value.to_string(),
    }
}

fn array<'a>(value: &'a Value, key: &str) -> io::Result<&'a [Value]> {
    match value.get(key) {
        None | Some(Value::Null) => Ok(&[]),
        Some(Value::Array(values)) => Ok(values),
        _ => Err(io::Error::new(
            io::ErrorKind::InvalidData,
            format!("{key} is not an array"),
        )),
    }
}

fn read_line(input: &mut impl BufRead) -> io::Result<Option<String>> {
    let mut line = String::new();
    if input.read_line(&mut line)? == 0 {
        Ok(None)
    } else {
        Ok(Some(
            line.trim_matches(['\0', '\t', '\n', '\x0b', '\x0c', '\r', ' '])
                .into(),
        ))
    }
}

fn to_i_index(line: &str) -> Option<usize> {
    let line = line.trim_start_matches(|c: char| c.is_ascii_whitespace());
    let negative = line.starts_with('-');
    let line = line.strip_prefix(['+', '-']).unwrap_or(line);
    let mut digits = String::new();
    let mut chars = line.chars().peekable();
    while let Some(ch) = chars.next() {
        if ch.is_ascii_digit() {
            digits.push(ch);
        } else if ch == '_' && !digits.is_empty() && chars.peek().is_some_and(char::is_ascii_digit)
        {
        } else {
            break;
        }
    }
    if digits.is_empty() || digits.bytes().all(|digit| digit == b'0') {
        Some(0)
    } else if negative {
        None
    } else {
        digits.parse().ok()
    }
}

fn pick_machine<'a>(
    machines: &'a [Value],
    requested: Option<&str>,
    config: &KexecNetbootConfig,
    interactive: bool,
    input: &mut impl BufRead,
    out: &mut impl Write,
    err: &mut impl Write,
) -> io::Result<Option<&'a Value>> {
    if machines.is_empty() {
        return Ok(None);
    }
    if requested.is_none() && interactive {
        let default = machines.iter().find(|machine| {
            machine.get("fqdn").and_then(Value::as_str) == Some(&config.default_fqdn)
        });
        loop {
            writeln!(out, "{:>5}  {:<30}  SPIN", "", "FQDN")?;
            let Some(default) = default else {
                // KEXEC-02: retain the original header and index-only effects.
                return Err(io::Error::other(format!(
                    "ERROR: Default machine '{}' not found.",
                    config.default_fqdn
                )));
            };
            for (index, machine) in machines.iter().enumerate() {
                let mark = if machine.get("fqdn") == default.get("fqdn") {
                    "*"
                } else {
                    ""
                };
                writeln!(
                    out,
                    "{:>5}  {:<30}  {}",
                    format!("[{}]{mark}", index + 1),
                    text(&machine["fqdn"]),
                    text(&machine["spin"])
                )?;
            }
            write!(out, "Select a machine by number: ")?;
            out.flush()?;
            let Some(line) = read_line(input)? else {
                return Ok(None);
            };
            match to_i_index(&line) {
                Some(0) => return Ok(Some(default)),
                Some(index) if index <= machines.len() => return Ok(Some(&machines[index - 1])),
                _ => writeln!(out, "Invalid selection. Please try again.")?,
            }
        }
    }
    let requested = requested.unwrap_or(&config.default_fqdn);
    let found = machines
        .iter()
        .find(|machine| machine.get("fqdn").and_then(Value::as_str) == Some(requested));
    if found.is_none() {
        writeln!(err, "Machine '{requested}' not found.")?;
    }
    Ok(found)
}

fn generation_input<'a>(input: &str, generations: &'a [Value]) -> Option<&'a Value> {
    let (negative, number) = super::ruby_integer::canonical(input)?;
    if negative {
        generations.get(number.parse::<usize>().ok()?)
    } else {
        generations.iter().find(|generation| {
            generation.get("generation").is_some_and(
                |value| matches!(value,Value::Number(value) if value.to_string()==number),
            )
        })
    }
}

fn pick_generation<'a>(
    machine: &'a Value,
    requested: Option<&str>,
    interactive: bool,
    input: &mut impl BufRead,
    out: &mut impl Write,
    err: &mut impl Write,
) -> io::Result<Option<&'a Value>> {
    let generations = array(machine, "generations")?;
    if generations.is_empty() {
        return Ok(None);
    }
    if let Some(requested) = requested {
        let selected = generation_input(requested, generations);
        if selected.is_none() {
            writeln!(
                err,
                "Requested generation '{requested}' not found (or invalid)."
            )?;
        }
        return Ok(selected);
    }
    let current = generations
        .iter()
        .find(|generation| generation.get("current") == Some(&Value::Bool(true)));
    if !interactive {
        return Ok(current.or_else(|| generations.first()));
    }
    loop {
        let default_label = match current {
            Some(current) => format!("(default is {} - current)", text(&current["generation"])),
            None => format!(
                "(no current labeled, default is newest: {})",
                text(&generations[0]["generation"])
            ),
        };
        writeln!(
            out,
            "Available generations for {}: {default_label}",
            text(&machine["fqdn"])
        )?;
        writeln!(out, "{:>5}  {:<19}  {:<10}  KERNEL", "", "TIME", "REVISION")?;
        for generation in generations {
            let mark = if generation
                .get("current")
                .is_some_and(|value| !value.is_null() && value != &Value::Bool(false))
            {
                "*"
            } else {
                ""
            };
            writeln!(
                out,
                "{:>5}  {:<19}  {:<10}  {}",
                format!("[{}]{mark}", text(&generation["generation"])),
                text(&generation["time_s"]),
                text(&generation["shortrev"]),
                text(&generation["kernel_version"])
            )?;
        }
        write!(
            out,
            "Enter generation number (or negative offset) [ENTER for default]: "
        )?;
        out.flush()?;
        let Some(line) = read_line(input)? else {
            return Ok(None);
        };
        if line.is_empty() {
            return Ok(current.or_else(|| generations.first()));
        }
        if let Some(selected) = generation_input(&line, generations) {
            return Ok(Some(selected));
        }
        writeln!(out, "Invalid generation '{line}'. Please try again.")?;
    }
}

fn pick_variant<'a>(
    generation: &'a Value,
    requested: Option<&str>,
    interactive: bool,
    input: &mut impl BufRead,
    out: &mut impl Write,
    err: &mut impl Write,
) -> io::Result<Option<&'a Value>> {
    let variants = array(generation, "variants")?;
    if variants.is_empty() {
        return Ok(None);
    }
    if let Some(requested) = requested {
        let selected = variants
            .iter()
            .find(|variant| variant.get("name").and_then(Value::as_str) == Some(requested));
        if selected.is_none() {
            writeln!(
                err,
                "Requested variant '{requested}' not found in generation {}.",
                text(&generation["generation"])
            )?;
        }
        return Ok(selected);
    }
    if !interactive {
        return Ok(variants.first());
    }
    loop {
        writeln!(
            out,
            "Variants available for generation {}:",
            text(&generation["generation"])
        )?;
        writeln!(out, "{:>5}  LABEL", "")?;
        for (index, variant) in variants.iter().enumerate() {
            writeln!(
                out,
                "{:>5}  {}",
                format!("[{}]", index + 1),
                text(&variant["label"])
            )?;
        }
        write!(
            out,
            "Choose variant by number [ENTER for '{}']: ",
            text(&variants[0]["name"])
        )?;
        out.flush()?;
        let Some(line) = read_line(input)? else {
            return Ok(None);
        };
        if line.is_empty() {
            return Ok(variants.first());
        }
        if let Some(index) =
            to_i_index(&line).filter(|index| *index > 0 && *index <= variants.len())
        {
            return Ok(Some(&variants[index - 1]));
        }
        writeln!(out, "Invalid selection. Please try again.")?;
    }
}

fn params(generation: &Value, variant: Option<&Value>, append: &str) -> io::Result<String> {
    let mut params = generation
        .get("kernel_params")
        .and_then(Value::as_array)
        .ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidData,
                "missing or invalid kernel_params",
            )
        })?
        .iter()
        .map(text)
        .collect::<Vec<_>>();
    if let Some(variant) = variant {
        params.extend(array(variant, "kernel_params")?.iter().map(text));
    }
    params.push(append.into());
    Ok(params.join(" "))
}

fn httproot(cmdline: &str) -> Option<&str> {
    cmdline.match_indices("httproot=").find_map(|(start, _)| {
        if start > 0
            && (cmdline.as_bytes()[start - 1].is_ascii_alphanumeric()
                || cmdline.as_bytes()[start - 1] == b'_')
        {
            return None;
        }
        let value = &cmdline[start + 9..];
        let end = value.find(char::is_whitespace).unwrap_or(value.len());
        (end > 0).then_some(&value[..end])
    })
}

fn index_url(root: &str, server: Option<&str>) -> io::Result<String> {
    if let Some(server) = server {
        return Ok(format!("{}/machines.json", server.trim_end_matches('/')));
    }
    let root = Url::parse(root).map_err(io::Error::other)?;
    let mut parts = root
        .path()
        .split('/')
        .filter(|part| !part.is_empty())
        .collect::<Vec<_>>();
    if parts.len() >= 3 {
        parts.truncate(parts.len() - 3);
    }
    let host = root
        .host_str()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "httproot has no host"))?;
    let host = if host.contains(':') && !host.starts_with('[') {
        format!("[{host}]")
    } else {
        host.to_owned()
    };
    let port = root.port_or_known_default().unwrap_or(80);
    let port = if port == 80 {
        String::new()
    } else {
        format!(":{port}")
    };
    let path = if parts.is_empty() {
        "/machines.json".into()
    } else {
        format!("/{}/machines.json", parts.join("/"))
    };
    // Preserve the old index derivation's URI::HTTP.build, even for https root.
    Ok(format!("http://{host}{port}{path}"))
}

fn client_builder() -> reqwest::ClientBuilder {
    Client::builder()
        .connect_timeout(Duration::from_secs(60))
        .read_timeout(Duration::from_secs(60))
        .no_gzip()
        .no_deflate()
        .no_brotli()
        .no_zstd()
        .redirect(reqwest::redirect::Policy::none())
}

fn client() -> io::Result<Client> {
    client_builder().build().map_err(io::Error::other)
}

async fn get(client: &Client, url: &str) -> io::Result<reqwest::Response> {
    let response = client
        .get(url)
        .header(ACCEPT_ENCODING, ACCEPTED_ENCODING)
        .send()
        .await
        .map_err(io::Error::other)?;
    if response.status().is_success() {
        Ok(response)
    } else {
        Err(io::Error::other(format!(
            "ERROR: Could not download {url}, HTTP {}",
            response.status().as_u16()
        )))
    }
}

async fn index(client: &Client, url: &str) -> io::Result<Value> {
    #[cfg(test)]
    super::helper_diagnostic_receipts::record(super::helper_diagnostic_receipts::Event::IndexGet { url: url.into() });
    let mut reader = body::Body::new(get(client, url).await?).await?;
    let mut body = Vec::new();
    reader.read_to_end(&mut body).await?;
    reader.finish_transport().await?;
    parse_index(&body)
}

fn parse_index(body: &[u8]) -> io::Result<Value> {
    serde_json::from_slice(body)
        .map_err(|source| io::Error::new(io::ErrorKind::InvalidData, IndexJsonFailure { source }))
}

async fn download(
    client: &Client,
    url: &str,
    out: &mut impl Write,
) -> io::Result<tempfile::TempPath> {
    let uri = Url::parse(url).map_err(io::Error::other)?;
    let basename = uri.path().rsplit('/').next().unwrap_or("");
    let mut file = tempfile::Builder::new().prefix(basename).tempfile()?;
    let transfer = async {
        writeln!(out, "Downloading {url} -> {}", file.path().display())?;
        out.flush()?;
        let mut response = body::Body::new(get(client, url).await?).await?;
        let mut buffer = [0u8; 16 * 1024];
        loop {
            let count = response.read(&mut buffer).await?;
            if count == 0 {
                break;
            }
            file.write_all(&buffer[..count])?;
        }
        response.finish_transport().await?;
        file.flush()?;
        Ok::<(), io::Error>(())
    }
    .await;
    let path = file.into_temp_path();
    match transfer {
        Ok(()) => Ok(path),
        Err(primary) => Err(cleanup_failed(primary, vec![path])),
    }
}

fn load_command(
    config: &KexecNetbootConfig,
    kernel: &Path,
    initrd: &Path,
    params: &str,
) -> Command {
    let mut initrd_argument = OsString::from("--initrd=");
    initrd_argument.push(initrd.as_os_str());
    let mut command = Command::new(&config.kexec);
    command
        .arg("--load")
        .arg(kernel)
        .arg(initrd_argument)
        .arg(format!("--append={params}"))
        .arg("--no-sync");
    command
}

fn checked_kexec(
    command: &mut Command,
    label: &str,
    printed: &str,
    out: &mut impl Write,
    err: &mut impl Write,
) -> io::Result<i32> {
    writeln!(out, "Executing: {printed}")?;
    out.flush()?;
    #[cfg(test)]
    super::helper_diagnostic_receipts::command_start(command);
    let status = command.status()?;
    #[cfg(test)]
    super::helper_diagnostic_receipts::command_wait(command, &status);
    if status.success() {
        writeln!(out, "kexec --{label} completed successfully.")?;
        Ok(0)
    } else {
        writeln!(err, "ERROR: kexec --{label} failed!")?;
        Ok(1)
    }
}

async fn run(
    config: &KexecNetbootConfig,
    options: Options,
    cmdline: &Path,
    input: &mut impl BufRead,
    out: &mut impl Write,
    err: &mut impl Write,
    http: &Client,
) -> io::Result<i32> {
    if options.exec || options.unload {
        let label = if options.unload { "unload" } else { "exec" };
        let mut command = Command::new(&config.kexec);
        command.arg(format!("--{label}")).arg("--no-sync");
        return checked_kexec(
            &mut command,
            label,
            &format!("{} --{label} --no-sync", config.kexec.display()),
            out,
            err,
        );
    }
    #[cfg(test)]
    super::helper_diagnostic_receipts::record(super::helper_diagnostic_receipts::Event::CmdlineRead { path: cmdline.into() });
    let cmdline = std::fs::read_to_string(cmdline)?;
    let Some(root) = httproot(&cmdline) else {
        writeln!(
            err,
            "ERROR: Could not find 'httproot=' parameter in /proc/cmdline"
        )?;
        return Ok(1);
    };
    let machines = index(http, &index_url(root, options.server_url.as_deref())?).await?;
    let Some(machine) = pick_machine(
        array(&machines, "machines")?,
        options.machine.as_deref(),
        config,
        options.interactive,
        input,
        out,
        err,
    )?
    else {
        writeln!(err, "ERROR: No suitable machine found.")?;
        return Ok(1);
    };
    let Some(generation) = pick_generation(
        machine,
        options.generation.as_deref(),
        options.interactive,
        input,
        out,
        err,
    )?
    else {
        writeln!(err, "ERROR: No generation found/selected.")?;
        return Ok(1);
    };
    let variant = pick_variant(
        generation,
        options.variant.as_deref(),
        options.interactive,
        input,
        out,
        err,
    )?;
    let params = params(generation, variant, &options.append)?;
    if options.interactive {
        writeln!(
            out,
            "\nSelected configuration:\n  Machine:       {} (spin={})\n  Generation:    {} ({}, rev={}, kernel={})",
            text(&machine["fqdn"]),
            text(&machine["spin"]),
            text(&generation["generation"]),
            text(&generation["time_s"]),
            text(&generation["shortrev"]),
            text(&generation["kernel_version"])
        )?;
        match variant {
            Some(variant) => writeln!(
                out,
                "  Variant:       {} ({})",
                text(&variant["name"]),
                text(&variant["label"])
            )?,
            None => writeln!(out, "  Variant:       none")?,
        };
        writeln!(out, "  Kernel params: {params}\n")?;
        loop {
            write!(out, "Continue? [y/N]: ")?;
            out.flush()?;
            let line = read_line(input)?.ok_or_else(|| {
                io::Error::new(io::ErrorKind::UnexpectedEof, "end of file reached")
            })?;
            match line.to_lowercase().as_str() {
                "y" => {
                    writeln!(out)?;
                    break;
                }
                "n" => {
                    writeln!(err, "Aborting")?;
                    return Ok(1);
                }
                _ => {}
            }
        }
    }
    let kernel_url = generation["boot_files"]["bzImage"]
        .as_str()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "missing kernel URL"))?;
    let kernel = download(http, kernel_url, out).await?;
    let Some(initrd_url) = generation["boot_files"]["initrd"].as_str() else {
        return Err(cleanup_failed(
            io::Error::new(io::ErrorKind::InvalidData, "missing initrd URL"),
            vec![kernel],
        ));
    };
    let initrd = match download(http, initrd_url, out).await {
        Ok(initrd) => initrd,
        Err(primary) => return Err(cleanup_failed(primary, vec![kernel])),
    };
    let result = checked_kexec(
        &mut load_command(config, &kernel, &initrd, &params),
        "load",
        &format!(
            "{} --load {} --initrd={} --append=\"{params}\" --no-sync",
            config.kexec.display(),
            kernel.display(),
            initrd.display()
        ),
        out,
        err,
    );
    let code = match result {
        Ok(code) => code,
        Err(primary) => return Err(cleanup_failed(primary, vec![kernel, initrd])),
    };
    let mut cleanup = Vec::new();
    for file in [kernel, initrd] {
        let path = file.to_path_buf();
        if let Err(error) = file.close() {
            cleanup.push((path, error));
        }
    }
    for index in 0..cleanup.len() {
        let (path, error) = &cleanup[index];
        if let Err(primary) = writeln!(
            err,
            "Warning: Could not remove tmp file {}: {error}",
            path.display()
        ) {
            return Err(io::Error::new(
                primary.kind(),
                TransferFailure { primary, cleanup },
            ));
        }
    }
    Ok(code)
}

struct KexecInputs<'a> {
    cmdline: &'a Path,
    #[cfg(test)]
    fixture_destination: Option<std::net::SocketAddrV4>,
}

impl KexecInputs<'_> {
    fn client(&self) -> io::Result<Client> {
        #[cfg(test)]
        if let Some(destination) = self.fixture_destination {
            return client_builder()
                .resolve("boot.fixture.invalid", std::net::SocketAddr::V4(destination))
                .build()
                .map_err(io::Error::other);
        }
        client()
    }
}

fn common_entry(program: &OsStr, args: &[OsString], inputs: &KexecInputs<'_>, input: &mut io::Stdin, stdout: &mut impl Write, stderr: &mut impl Write) -> i32 {
    let result = (|| -> io::Result<i32> {
        let options = match parse(args)? {
            Parsed::Help => {
                stdout.write_all(help(program).as_bytes())?;
                return Ok(0);
            }
            Parsed::Run(options) => options,
        };
        if options.exec && options.unload {
            writeln!(
                stderr,
                "ERROR: use either --unload or --exec, not both"
            )?;
            return Ok(1);
        }
        let config = KexecNetbootConfig::from_package()?;
        if options.exec || options.unload {
            let label = if options.unload { "unload" } else { "exec" };
            let mut command = Command::new(&config.kexec);
            command.arg(format!("--{label}")).arg("--no-sync");
            return checked_kexec(
                &mut command,
                label,
                &format!("{} --{label} --no-sync", config.kexec.display()),
                stdout,
                stderr,
            );
        }
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()?;
        runtime.block_on(run(
            &config,
            options,
            inputs.cmdline,
            &mut input.lock(),
            stdout,
            stderr,
            &inputs.client()?,
        ))
    })();
    finish(result, stderr)
}


fn finish(result: io::Result<i32>, stderr: &mut impl Write) -> i32 {
    match result {
        Ok(code) => code,
        Err(error) => {
            let _ = writeln!(stderr, "{error}");
            1
        }
    }
}
