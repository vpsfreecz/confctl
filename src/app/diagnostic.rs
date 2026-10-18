//! Explicit byte-oriented Nix diagnostics. General error Display stays redacted.

use crate::{
    error::{ConfctlError, NixCommandContext},
    process::{Captured, PartialProcessEvidence},
};
use std::io::{self, Read, Write};

pub(super) fn command(writer: &mut impl Write, context: &NixCommandContext) -> io::Result<()> {
    writer.write_all(b"cd ")?;
    shellword(writer, context.cwd.as_os_str().as_encoded_bytes())?;
    writer.write_all(b" && ")?;
    shellword(writer, context.program.as_encoded_bytes())?;
    for arg in &context.args {
        writer.write_all(b" ")?;
        shellword(writer, arg.as_encoded_bytes())?;
    }
    Ok(())
}

// Ruby Shellwords.shellescape: ASCII-safe set and a quoted literal newline.
fn shellword(writer: &mut impl Write, bytes: &[u8]) -> io::Result<()> {
    if bytes.is_empty() {
        return writer.write_all(b"''");
    }
    if let Ok(value) = std::str::from_utf8(bytes) {
        for character in value.chars() {
            if character == '\n' {
                writer.write_all(b"'\n'")?;
            } else {
                if !(character.is_ascii_alphanumeric() || "_-.,:+/@".contains(character)) {
                    writer.write_all(b"\\")?;
                }
                let mut encoded = [0; 4];
                writer.write_all(character.encode_utf8(&mut encoded).as_bytes())?;
            }
        }
        return Ok(());
    }
    for &byte in bytes {
        if byte == b'\n' {
            writer.write_all(b"'\n'")?;
        } else {
            if !(byte.is_ascii_alphanumeric() || b"_-.,:+/@".contains(&byte)) {
                writer.write_all(b"\\")?;
            }
            writer.write_all(&[byte])?;
        }
    }
    Ok(())
}

pub(super) fn copy_capture(capture: &Captured, writer: &mut impl Write) -> io::Result<()> {
    let copied = match capture {
        Captured::Memory(bytes) => {
            writer.write_all(bytes)?;
            bytes.len() as u64
        }
        Captured::Spool { file, length } => io::copy(&mut file.reopen()?.take(*length), writer)?,
        Captured::Unavailable => 0,
    };
    if copied != capture.len() {
        return Err(io::Error::new(
            io::ErrorKind::UnexpectedEof,
            "short process capture",
        ));
    }
    Ok(())
}

pub(super) fn write_error(writer: &mut impl Write, error: &ConfctlError) -> io::Result<()> {
    write_inner(writer, error, None, b"error: ")?;
    writer.flush()
}
pub(super) fn write_redacted(writer: &mut impl Write, error: &ConfctlError) -> io::Result<()> {
    match error {
        ConfctlError::NixCommandFailure(failure) => write_redacted(writer, &failure.source),
        ConfctlError::ProcessFailure { source, .. }
        | ConfctlError::OutputFailure { source, .. } => write_redacted(writer, source),
        ConfctlError::Cleanup { primary, secondary } => {
            write_redacted(writer, primary)?;
            for error in secondary {
                writeln!(writer, "cleanup: {error}")?;
            }
            writer.flush()
        }
        _ => {
            writeln!(writer, "error: {error}")?;
            writer.flush()
        }
    }
}

fn write_inner(
    writer: &mut impl Write,
    error: &ConfctlError,
    context: Option<&NixCommandContext>,
    prefix: &[u8],
) -> io::Result<()> {
    match error {
        ConfctlError::NixCommandFailure(failure) => {
            write_inner(writer, &failure.source, Some(&failure.context), prefix)
        }
        ConfctlError::Cleanup { primary, secondary } => {
            write_inner(writer, primary, context, prefix)?;
            for error in secondary {
                write_inner(writer, error, None, b"cleanup: ")?;
            }
            Ok(())
        }
        ConfctlError::ProcessFailure { source, evidence } => {
            if let Some(context) = context
                && let ConfctlError::Child { code, signal, .. } = primary(source)
                && evidence.stdout_complete
                && evidence.stderr_complete
                && !matches!(evidence.stdout, Captured::Unavailable)
                && !matches!(evidence.stderr, Captured::Unavailable)
                && evidence.status.is_some_and(|status| !status.success())
                && let Some(status) = code.or(*signal)
            {
                // Read before publishing the complete-child layout. A failed
                // read must not turn missing bytes into "Nothing written".
                let stdout = capture_bytes(&evidence.stdout)?;
                let stderr = capture_bytes(&evidence.stderr)?;
                writer.write_all(prefix)?;
                writer.write_all(b"Running `")?;
                command(writer, context)?;
                writeln!(writer, "` failed with\n  exit status: {status}")?;
                writer.write_all(b"  stdout: ")?;
                output(writer, &stdout)?;
                writer.write_all(b"\n  stderr: ")?;
                output(writer, &stderr)?;
                writer.write_all(b"\n")?;
                write_secondary(writer, source)
            } else {
                write_inner(writer, source, context, prefix)
            }
        }
        ConfctlError::OutputFailure { source, .. } => write_inner(writer, source, context, prefix),
        _ => {
            writer.write_all(prefix)?;
            writeln!(writer, "{error}")
        }
    }
}

fn primary(error: &ConfctlError) -> &ConfctlError {
    match error {
        ConfctlError::Cleanup { primary: inner, .. } => primary(inner),
        _ => error,
    }
}
fn write_secondary(writer: &mut impl Write, error: &ConfctlError) -> io::Result<()> {
    if let ConfctlError::Cleanup { primary, secondary } = error {
        write_secondary(writer, primary)?;
        for error in secondary {
            write_inner(writer, error, None, b"cleanup: ")?;
        }
    }
    Ok(())
}

fn capture_bytes(capture: &Captured) -> io::Result<Vec<u8>> {
    let mut bytes = Vec::new();
    copy_capture(capture, &mut bytes)?;
    Ok(bytes)
}
fn output(writer: &mut impl Write, bytes: &[u8]) -> io::Result<()> {
    let strip = |byte: &u8| *byte == 0 || b" \t\r\n\x0b\x0c".contains(byte);
    // Ruby String#strip additionally removes NUL. Use native bytes throughout.
    let begin = bytes
        .iter()
        .position(|byte| !strip(byte))
        .unwrap_or(bytes.len());
    let end = bytes
        .iter()
        .rposition(|byte| !strip(byte))
        .map_or(begin, |n| n + 1);
    let bytes = &bytes[begin..end];
    writer.write_all(if bytes.is_empty() {
        b"Nothing written"
    } else {
        bytes
    })
}

pub(super) fn fatal_evidence(writer: &mut impl Write, error: &ConfctlError) -> io::Result<()> {
    match error {
        ConfctlError::NixCommandFailure(failure) => {
            writer.write_all(b"\n[fatal Nix command] ")?;
            command(writer, &failure.context)?;
            writer.write_all(b"\n")?;
            fatal_evidence(writer, &failure.source)
        }
        ConfctlError::ProcessFailure { source, evidence } => {
            process_evidence(writer, evidence)?;
            fatal_evidence(writer, source)
        }
        ConfctlError::OutputFailure { source, .. } => fatal_evidence(writer, source),
        ConfctlError::Cleanup { primary, secondary } => {
            fatal_evidence(writer, primary)?;
            for (index, error) in secondary.iter().enumerate() {
                writeln!(writer, "\n[ordered cleanup {}]", index + 1)?;
                fatal_evidence(writer, error)?;
            }
            Ok(())
        }
        _ => writeln!(writer, "[fatal source] {error}"),
    }
}
fn process_evidence(writer: &mut impl Write, evidence: &PartialProcessEvidence) -> io::Result<()> {
    use std::os::unix::process::ExitStatusExt;
    match evidence.status {
        Some(status) => writeln!(
            writer,
            "[native status] exit={:?} signal={:?}",
            status.code(),
            status.signal()
        )?,
        None => writer.write_all(b"[native status] unavailable\n")?,
    }
    for (name, capture, complete) in [
        ("stdout", &evidence.stdout, evidence.stdout_complete),
        ("stderr", &evidence.stderr, evidence.stderr_complete),
    ] {
        writeln!(
            writer,
            "[{name}] complete={complete} available={} captured_bytes={}",
            !matches!(capture, Captured::Unavailable),
            capture.len()
        )?;
        copy_capture(capture, writer)?;
        writeln!(writer, "\n[end {name}]")?;
    }
    Ok(())
}

#[cfg(test)]
pub(super) mod tests {
    use super::*;
    use crate::error::NixCommandFailure;
    use std::{
        ffi::OsString,
        os::unix::{ffi::OsStringExt, process::ExitStatusExt},
    };

    pub(in crate::app) fn failure() -> ConfctlError {
        ConfctlError::NixCommandFailure(Box::new(NixCommandFailure {
            context: NixCommandContext {
                cwd: "/config space".into(),
                program: "nix".into(),
                args: vec!["eval".into(), ".#settings".into()],
            },
            source: Box::new(ConfctlError::ProcessFailure {
                source: Box::new(ConfctlError::Child {
                    description: "nix eval".into(),
                    code: Some(7),
                    signal: None,
                    log: None,
                }),
                evidence: Box::new(PartialProcessEvidence {
                    status: Some(std::process::ExitStatus::from_raw(7 << 8)),
                    stdout: Captured::Memory(vec![]),
                    stderr: Captured::Memory(b" \0failure\xff \n".to_vec()),
                    stdout_complete: true,
                    stderr_complete: true,
                    log: None,
                }),
            }),
        }))
    }
    #[test]
    fn exact_command_failure_uses_native_bytes_and_ruby_strip() {
        let mut bytes = vec![];
        write_error(&mut bytes, &failure()).unwrap();
        assert_eq!(bytes,b"error: Running `cd /config\\ space && nix eval .\\#settings` failed with\n  exit status: 7\n  stdout: Nothing written\n  stderr: failure\xff\n");
    }
    #[test]
    fn nonempty_stdout_and_whitespace_stderr_keep_exact_stream_order() {
        let ConfctlError::NixCommandFailure(mut failure) = failure() else {
            unreachable!()
        };
        let ConfctlError::ProcessFailure { evidence, .. } = failure.source.as_mut() else {
            unreachable!()
        };
        evidence.stdout = Captured::Memory(b"\nfirst\nsecond\0".to_vec());
        evidence.stderr = Captured::Memory(b"\0 \t\r\n\x0b\x0c".to_vec());
        let mut bytes = vec![];
        write_error(&mut bytes, &ConfctlError::NixCommandFailure(failure)).unwrap();
        assert!(bytes.ends_with(b"  stdout: first\nsecond\n  stderr: Nothing written\n"));
    }
    #[test]
    fn shellwords_preserve_empty_quotes_newline_and_non_utf8() {
        let context = NixCommandContext {
            cwd: "/x".into(),
            program: "nix".into(),
            args: vec!["".into(), "a'b\nc$".into(), OsString::from_vec(vec![0xff])],
        };
        let mut bytes = vec![];
        command(&mut bytes, &context).unwrap();
        assert_eq!(bytes, b"cd /x && nix '' a\\'b'\n'c\\$ \\\xff");
    }
    #[test]
    fn native_signal_and_cleanup_order_and_partial_refusal() {
        let ConfctlError::NixCommandFailure(mut failure) = failure() else {
            unreachable!()
        };
        let ConfctlError::ProcessFailure { source, evidence } = failure.source.as_mut() else {
            unreachable!()
        };
        **source = ConfctlError::Child {
            description: "nix eval".into(),
            code: None,
            signal: Some(15),
            log: None,
        };
        evidence.status = Some(std::process::ExitStatus::from_raw(15));
        let mut bytes = vec![];
        write_error(&mut bytes, &ConfctlError::NixCommandFailure(failure)).unwrap();
        assert!(bytes.windows(15).any(|s| s == b"exit status: 15"));
        let ConfctlError::NixCommandFailure(mut failure) = self::failure() else {
            unreachable!()
        };
        let ConfctlError::ProcessFailure { evidence, .. } = failure.source.as_mut() else {
            unreachable!()
        };
        evidence.stderr_complete = false;
        let error = ConfctlError::NixCommandFailure(failure)
            .with_secondary(vec![ConfctlError::InputEof, ConfctlError::Cancelled]);
        bytes.clear();
        write_error(&mut bytes, &error).unwrap();
        assert_eq!(bytes,b"error: nix eval failed (exit Some(7), signal None)\ncleanup: end of input\ncleanup: operation canceled\n");
    }
    #[test]
    fn short_capture_and_broken_diagnostic_writer_are_errors() {
        struct Broken;
        impl Write for Broken {
            fn write(&mut self, _: &[u8]) -> io::Result<usize> {
                Err(io::ErrorKind::BrokenPipe.into())
            }
            fn flush(&mut self) -> io::Result<()> {
                Ok(())
            }
        }
        assert!(write_error(&mut Broken, &failure()).is_err());
        let file = tempfile::NamedTempFile::new().unwrap();
        assert!(copy_capture(&Captured::Spool { file, length: 1 }, &mut vec![]).is_err());
    }
}
