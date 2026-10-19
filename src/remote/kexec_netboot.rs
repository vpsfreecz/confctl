//! Standalone netboot index reader and kexec client. HTTP/image protocols and
//! selection order follow the installed Ruby helper. HTTPS-01 verifies image
//! TLS; KEXEC-01 passes one literal append argument; KEXEC-02 owns only the
//! interactive missing-default diagnostic. No shell interprets metadata.

mod body;

include!("kexec_netboot/common.rs");

pub fn entry() -> i32 {
    let mut args = std::env::args_os();
    let program = args.next().unwrap_or_else(|| "kexec-netboot".into());
    let args = args.collect::<Vec<_>>();
    common_entry(
        &program,
        &args,
        &KexecInputs {
            cmdline: Path::new("/proc/cmdline"),
            #[cfg(test)]
            fixture_destination: None,
        },
        &mut io::stdin(),
        &mut io::stdout(),
        &mut io::stderr(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::os::unix::fs::PermissionsExt;

    fn config() -> KexecNetbootConfig {
        KexecNetbootConfig {
            default_fqdn: "node.invalid".into(),
            kexec: "/must/not/execute".into(),
        }
    }

    #[test]
    fn options_consume_clusters_and_attached_or_separate_values() {
        for arguments in [
            vec![
                "-iy",
                "-mnode.invalid",
                "-g",
                "-1",
                "-v=default",
                "-a",
                "two words",
                "-shttp://boot.invalid",
            ],
            vec![
                "--inter",
                "--no-sync",
                "--machine=node.invalid",
                "--generation=-1",
                "--variant",
                "default",
                "--append=two words",
                "--server-url",
                "http://boot.invalid",
            ],
            vec![
                "-iymnode.invalid",
                "-g-1",
                "-vdefault",
                "-atwo words",
                "-s",
                "http://boot.invalid",
            ],
        ] {
            let args = arguments
                .into_iter()
                .map(OsString::from)
                .collect::<Vec<_>>();
            let Parsed::Run(options) = parse(&args).unwrap() else {
                panic!("expected options")
            };
            assert!(options.interactive);
            assert_eq!(options.machine.as_deref(), Some("node.invalid"));
            assert_eq!(options.generation.as_deref(), Some("-1"));
            assert_eq!(options.variant.as_deref(), Some("default"));
            assert_eq!(options.append, "two words");
            assert_eq!(options.server_url.as_deref(), Some("http://boot.invalid"));
        }
        let Parsed::Run(options) = parse(&["-eu".into()]).unwrap() else {
            panic!("expected options")
        };
        assert!(options.exec && options.unload); // The entrypoint rejects the conflict before effects.
        for argument in [
            "-eZ",
            "--exec=garbage",
            "--unload=",
            "--interactive=yes",
            "--no-sync=false",
            "-y=garbage",
            "-eé",
        ] {
            assert!(
                matches!(parse(&[argument.into()]), Err(error) if error.kind() == io::ErrorKind::InvalidInput),
                "{argument}"
            );
        }
        assert!(matches!(parse(&["-h".into()]), Ok(Parsed::Help)));
    }

    async fn stalled_http_error(prefix: &'static [u8], image: bool) -> io::Error {
        use tokio::{io::AsyncWriteExt, net::TcpListener, sync::oneshot};

        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let (release, released) = oneshot::channel();
        let server = tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            let mut request = Vec::new();
            // The peer deliberately withholds headers or the rest of the body.
            tokio::time::timeout(Duration::from_secs(3), async {
                while !request.ends_with(b"\r\n\r\n") {
                    request.push(socket.read_u8().await.unwrap());
                }
                socket.write_all(prefix).await.unwrap();
            })
            .await
            .unwrap();
            let _ = released.await;
        });
        let http = client_builder()
            .no_proxy()
            .connect_timeout(Duration::from_millis(100))
            .read_timeout(Duration::from_millis(100))
            .build()
            .unwrap();
        let url = format!("http://{address}/image");
        let mut output = Vec::new();
        let result = tokio::time::timeout(Duration::from_secs(3), async {
            if image {
                download(&http, &url, &mut output).await.map(|_| ())
            } else {
                index(&http, &url).await.map(|_| ())
            }
        })
        .await;
        let _ = release.send(());
        server.await.unwrap();
        let error = result
            .expect("client must fail before the test guard")
            .unwrap_err();
        if image {
            let transfer = error
                .get_ref()
                .unwrap()
                .downcast_ref::<TransferFailure>()
                .unwrap();
            assert!(transfer.cleanup.is_empty());
            let output = String::from_utf8(output).unwrap();
            let (_, path) = output.lines().next().unwrap().split_once(" -> ").unwrap();
            assert!(!Path::new(path).exists());
        }
        error
    }

    fn assert_http_timeout(error: &io::Error) {
        let mut source: &(dyn std::error::Error + 'static) = error;
        loop {
            if let Some(error) = source.downcast_ref::<reqwest::Error>() {
                assert!(error.is_timeout(), "{error}");
                return;
            }
            source = source
                .downcast_ref::<io::Error>()
                .and_then(io::Error::get_ref)
                .map(|inner| inner as &(dyn std::error::Error + 'static))
                .or_else(|| source.source())
                .unwrap_or_else(|| panic!("retained reqwest timeout cause: {error:?}"));
        }
    }

    #[tokio::test]
    async fn index_header_stall_has_finite_read_timeout() {
        assert_http_timeout(&stalled_http_error(b"", false).await);
    }

    #[tokio::test]
    async fn image_body_stall_has_finite_read_timeout_and_removes_partial_file() {
        assert_http_timeout(
            &stalled_http_error(b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nx", true).await,
        );
    }

    #[test]
    fn failed_transfer_retains_primary_and_ordered_cleanup_locator_without_debug_data() {
        let dir = tempfile::tempdir().unwrap();
        let file = tempfile::NamedTempFile::new_in(dir.path())
            .unwrap()
            .into_temp_path();
        let path = file.to_path_buf();
        std::fs::remove_file(&path).unwrap();
        std::fs::create_dir(&path).unwrap();
        let error = cleanup_failed(
            io::Error::new(io::ErrorKind::UnexpectedEof, "private-primary"),
            vec![file],
        );
        let source = error
            .get_ref()
            .unwrap()
            .downcast_ref::<TransferFailure>()
            .unwrap();
        assert_eq!(source.primary.kind(), io::ErrorKind::UnexpectedEof);
        assert_eq!(source.cleanup.len(), 1);
        assert_eq!(source.cleanup[0].0, path);
        assert!(std::error::Error::source(source).is_some());
        let debug = format!("{error:?}");
        assert!(!debug.contains("private-primary"));
        assert!(!debug.contains(dir.path().to_str().unwrap()));
        std::fs::remove_dir(path).unwrap();
    }

    #[test]
    fn kexec_03_bad_object_keeps_exact_owned_suffix_and_structured_parse_source() {
        let rows: Value = serde_json::from_str(include_str!(concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/tests/compat/helpers/candidate/diagnostics-v1.json"
        )))
        .unwrap();
        let error = parse_index(b"{bad").unwrap_err();
        let failure = error
            .get_ref()
            .unwrap()
            .downcast_ref::<IndexJsonFailure>()
            .unwrap();
        assert_eq!(
            failure.source.classify(),
            serde_json::error::Category::Syntax
        );
        assert_eq!((failure.source.line(), failure.source.column()), (1, 2));
        assert!(std::error::Error::source(failure).is_some());
        assert!(!format!("{error:?}").contains("bad"));
        let mut stderr = Vec::new();
        assert_eq!(finish(Err(error), &mut stderr), 1);
        assert_eq!(
            stderr,
            rows["rows"]["kexec-json-failure"]["expected"]["stderr"]
                .as_str()
                .unwrap()
                .as_bytes()
        );
        // Empty/EOF and other syntax cases have no promoted expectations here.
        // Transport and decoder errors never enter parse_index.
    }

    #[test]
    fn index_derivation_generation_offsets_variants_and_trailing_append_match_original() {
        assert_eq!(
            index_url("http://boot.invalid/prefix/node/9/root.squashfs", None).unwrap(),
            "http://boot.invalid/prefix/machines.json"
        );
        assert_eq!(
            index_url("https://boot.invalid/node/9/root.squashfs", None).unwrap(),
            "http://boot.invalid:443/machines.json"
        );
        assert_eq!(
            index_url("ignored", Some("http://override.invalid/")).unwrap(),
            "http://override.invalid/machines.json"
        );
        let generations = json!([{"generation":9,"current":false},{"generation":8,"current":true},{"generation":6}]);
        let generations = generations.as_array().unwrap();
        assert_eq!(
            generation_input("-1", generations).unwrap()["generation"],
            8
        );
        assert!(generation_input("-3", generations).is_none());
        assert_eq!(
            generation_input("0x9", generations).unwrap()["generation"],
            9
        );
        assert!(generation_input("not-a-number", generations).is_none());
        let generation = json!({"generation":9,"kernel_params":["console=ttyS0"],"variants":[{"name":"default","kernel_params":["runlevel=default"]}]});
        let (mut input, mut out, mut err) = (io::Cursor::new(b""), Vec::new(), Vec::new());
        let variant = pick_variant(
            &generation,
            Some("missing"),
            false,
            &mut input,
            &mut out,
            &mut err,
        )
        .unwrap();
        assert!(variant.is_none());
        assert_eq!(params(&generation, variant, "").unwrap(), "console=ttyS0 ");
        assert_eq!(
            err,
            b"Requested variant 'missing' not found in generation 9.\n"
        );
    }

    #[test]
    fn kexec_02_missing_interactive_default_keeps_exact_header_and_owned_error() {
        let machines = json!([{"fqdn":"other.invalid","spin":"vpsadminos"}]);
        let (mut input, mut out, mut err) = (io::Cursor::new(b"\n"), Vec::new(), Vec::new());
        let error = pick_machine(
            machines.as_array().unwrap(),
            None,
            &config(),
            true,
            &mut input,
            &mut out,
            &mut err,
        )
        .unwrap_err();
        assert_eq!(out, b"       FQDN                            SPIN\n");
        assert_eq!(
            format!("{error}\n"),
            "ERROR: Default machine 'node.invalid' not found.\n"
        );
        assert!(err.is_empty());
        assert_eq!(input.position(), 0);
        let mut empty = config();
        empty.default_fqdn.clear();
        let machines = json!([{"fqdn":"","spin":"nixos"}]);
        assert!(
            pick_machine(
                machines.as_array().unwrap(),
                None,
                &empty,
                true,
                &mut input,
                &mut out,
                &mut err
            )
            .unwrap()
            .is_some()
        );
    }

    #[test]
    fn native_kexec_01_literal_append_is_one_argument_and_cannot_run_shell_sentinels() {
        let dir = tempfile::tempdir().unwrap();
        let executable = dir.path().join("kexec");
        let record = dir.path().join("argv");
        let sentinel = dir.path().join("unexpected");
        let record_quote = format!("'{}'", record.to_str().unwrap().replace('\'', "'\\''"));
        std::fs::write(
            &executable,
            format!("#!/bin/sh\nprintf '%s\\000' \"$@\" > {record_quote}\nexit 0\n"),
        )
        .unwrap();
        std::fs::set_permissions(&executable, std::fs::Permissions::from_mode(0o755)).unwrap();
        let params = format!(
            "literal=$HOME command=$(touch {}) backtick=`touch {}` quote=\"q\" backslash=\\ semi=; two words ",
            sentinel.display(),
            sentinel.display()
        );
        let config = KexecNetbootConfig {
            default_fqdn: String::new(),
            kexec: executable,
        };
        let mut command =
            load_command(&config, Path::new("/kernel"), Path::new("/initrd"), &params);
        let (mut out, mut err) = (Vec::new(), Vec::new());
        assert_eq!(
            checked_kexec(
                &mut command,
                "load",
                "legacy diagnostic only",
                &mut out,
                &mut err
            )
            .unwrap(),
            0
        );
        let expected = [
            "--load",
            "/kernel",
            "--initrd=/initrd",
            &format!("--append={params}"),
            "--no-sync",
        ]
        .join("\0")
            + "\0";
        assert_eq!(std::fs::read(record).unwrap(), expected.as_bytes());
        assert!(!sentinel.exists());
        assert!(err.is_empty());
        assert_eq!(
            out,
            b"Executing: legacy diagnostic only\nkexec --load completed successfully.\n"
        );
    }
}
