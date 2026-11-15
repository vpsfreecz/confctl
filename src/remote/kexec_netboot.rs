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

    fn recording_kexec(directory: &Path) -> PathBuf {
        let executable = directory.join("kexec");
        // A waited writer avoids inheriting an open executable across forks.
        let status = Command::new("/bin/sh")
            .args(["-c", "set -C; printf '%s' \"$2\" > \"$1\"", "write-fixture"])
            .arg(&executable)
            .arg("#!/bin/sh\nprintf '%s\\000' \"$@\" > \"$0.argv\"\nexit 0\n")
            .status()
            .unwrap();
        assert!(status.success());
        std::fs::set_permissions(&executable, std::fs::Permissions::from_mode(0o755)).unwrap();
        executable
    }

    #[tokio::test]
    async fn exec_and_unload_keep_no_sync_without_cmdline_or_http() {
        let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        listener.set_nonblocking(true).unwrap();
        let http = client_builder().no_proxy().build().unwrap();
        for action in ["--exec", "--unload"] {
            let dir = tempfile::tempdir().unwrap();
            let config = KexecNetbootConfig {
                default_fqdn: String::new(),
                kexec: recording_kexec(dir.path()),
            };
            let arguments = [
                action.into(),
                "--no-sync".into(),
                "--server-url".into(),
                format!("http://{}", listener.local_addr().unwrap()).into(),
            ];
            let Parsed::Run(options) = parse(&arguments).unwrap() else {
                panic!("options")
            };
            let (mut out, mut err) = (Vec::new(), Vec::new());
            assert_eq!(
                run(
                    &config,
                    options,
                    &dir.path().join("missing-cmdline"),
                    &mut io::Cursor::new(b""),
                    &mut out,
                    &mut err,
                    &http
                )
                .await
                .unwrap(),
                0
            );
            assert_eq!(
                std::fs::read(dir.path().join("kexec.argv")).unwrap(),
                format!("{action}\0--no-sync\0").as_bytes()
            );
            assert!(err.is_empty());
            assert_eq!(
                listener.accept().unwrap_err().kind(),
                io::ErrorKind::WouldBlock
            );
        }
    }

    #[tokio::test]
    async fn interactive_defaults_decline_and_index_failures_preserve_effects() {
        use tokio::{io::AsyncWriteExt, net::TcpListener, sync::oneshot};

        // Replaces the finite ordinary Ruby oracle's selection/effect assertions.
        for (arguments, input, expected_code, has_httproot, http_status) in [
            (vec!["-i", "-a", "debug=1"], "\n\n\ny\n", 0, true, 200),
            (
                vec!["-i", "-m", "node.invalid", "-g", "8", "-v", "rescue"],
                "n\n",
                1,
                true,
                200,
            ),
            (vec!["-g", "-3"], "", 1, true, 200),
            (vec!["--server-url"], "", 1, false, 200),
            (vec![], "", 1, true, 404),
        ] {
            let dir = tempfile::tempdir().unwrap();
            let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
            let address = listener.local_addr().unwrap();
            let generations = [9, 8, 6].map(|number| json!({
                "generation":number,"current":number == 8,"time_s":"fixture-time",
                "shortrev":"012345678","kernel_version":"6.12.3",
                "kernel_params":["console=ttyS0"],
                "variants":[{"name":"default","label":"Default","kernel_params":["runlevel=default"]},
                            {"name":"rescue","label":"Rescue","kernel_params":["runlevel=rescue"]}],
                "boot_files":{"bzImage":format!("http://{address}/node/{number}/bzImage"),
                              "initrd":format!("http://{address}/node/{number}/initrd")}
            }));
            let index_body = serde_json::to_vec(&json!({"machines":[{
                "fqdn":"node.invalid","spin":"vpsadminos","generations":generations
            }]}))
            .unwrap();
            let (stop, mut stopped) = oneshot::channel();
            let server = tokio::spawn(async move {
                let mut paths = Vec::new();
                loop {
                    let (mut socket, _) = tokio::select! {
                        _ = &mut stopped => break,
                        accepted = listener.accept() => accepted.unwrap(),
                    };
                    tokio::time::timeout(Duration::from_secs(3), async {
                        let mut request = Vec::new();
                        while !request.ends_with(b"\r\n\r\n") {
                            assert!(request.len() < 65536);
                            request.push(socket.read_u8().await.unwrap());
                        }
                        let request = String::from_utf8(request).unwrap();
                        let path = request.lines().next().unwrap().split_whitespace().nth(1).unwrap();
                        paths.push(path.to_owned());
                        let body = if path == "/machines.json" { index_body.as_slice() } else { b"boot-image" };
                        socket.write_all(format!("HTTP/1.1 {http_status} fixture\r\nConnection: close\r\nContent-Length: {}\r\n\r\n", body.len()).as_bytes()).await.unwrap();
                        socket.write_all(body).await.unwrap();
                    }).await.unwrap();
                }
                paths
            });
            let cmdline = dir.path().join("cmdline");
            let cmdline_text = if has_httproot {
                format!("httproot=http://{address}/node/8/root.squashfs")
            } else {
                "console=ttyS0".into()
            };
            std::fs::write(&cmdline, cmdline_text).unwrap();
            let config = KexecNetbootConfig {
                default_fqdn: "node.invalid".into(),
                kexec: recording_kexec(dir.path()),
            };
            let mut arguments = arguments
                .into_iter()
                .map(OsString::from)
                .collect::<Vec<_>>();
            if !has_httproot {
                arguments.push(format!("http://{address}").into());
            }
            let Parsed::Run(options) = parse(&arguments).unwrap() else {
                panic!("options")
            };
            let http = client_builder().no_proxy().build().unwrap();
            let (mut out, mut err) = (Vec::new(), Vec::new());
            let result = tokio::time::timeout(
                Duration::from_secs(3),
                run(
                    &config,
                    options,
                    &cmdline,
                    &mut io::Cursor::new(input),
                    &mut out,
                    &mut err,
                    &http,
                ),
            )
            .await;
            let _ = stop.send(());
            let paths = server.await.unwrap();
            assert_eq!(
                finish(result.unwrap(), &mut err),
                expected_code,
                "arguments={arguments:?}; requests={paths:?}; stderr={}; stdout={}",
                String::from_utf8_lossy(&err),
                String::from_utf8_lossy(&out),
            );
            let out = String::from_utf8(out).unwrap();
            let err = String::from_utf8(err).unwrap();
            let record = dir.path().join("kexec.argv");
            if expected_code == 0 {
                assert_eq!(
                    paths,
                    ["/machines.json", "/node/8/bzImage", "/node/8/initrd"]
                );
                for prompt in [
                    "Select a machine",
                    "Available generations",
                    "Choose variant",
                    "Continue?",
                ] {
                    assert!(out.contains(prompt), "{out}");
                }
                assert!(out.contains("Machine:       node.invalid"));
                assert!(out.contains("Generation:    8"));
                assert!(out.contains("Variant:       default"));
                assert!(err.is_empty());
                let bytes = std::fs::read(record).unwrap();
                let args = bytes
                    .split(|byte| *byte == 0)
                    .filter(|arg| !arg.is_empty())
                    .collect::<Vec<_>>();
                assert_eq!(args.len(), 5);
                assert_eq!(args[0], b"--load");
                assert!(args[2].starts_with(b"--initrd="));
                assert_eq!(args[3], b"--append=console=ttyS0 runlevel=default debug=1");
                assert_eq!(args[4], b"--no-sync");
                let paths = out
                    .lines()
                    .filter_map(|line| line.strip_prefix("Downloading "))
                    .map(|line| line.split_once(" -> ").unwrap().1)
                    .collect::<Vec<_>>();
                assert_eq!(paths.len(), 2);
                for path in paths {
                    assert!(!Path::new(path).exists());
                }
            } else {
                if has_httproot {
                    assert_eq!(paths, ["/machines.json"]);
                } else {
                    assert!(paths.is_empty(), "explicit server must not bypass httproot");
                }
                assert!(!record.exists());
                assert!(!out.contains("Downloading "));
                if !has_httproot {
                    assert!(err.contains("Could not find 'httproot='"));
                } else if http_status == 404 {
                    assert!(err.contains("HTTP 404"));
                    assert!(!err.contains("Could not parse JSON"));
                } else if input == "n\n" {
                    assert_eq!(err, "Aborting\n");
                } else {
                    assert!(err.contains("not found (or invalid)"));
                    assert!(err.contains("No generation found/selected"));
                }
            }
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

    // RFC 1952 member for {"machines":[]}; FLG.FHCRC is set and the low
    // 16 bits of the header's CRC32 are 0xab12 (stored little endian).
    const GZIP_FHCRC: &[u8] = &[
        0x1f, 0x8b, 0x08, 0x02, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0x12, 0xab, 0xab, 0x56, 0xca,
        0x4d, 0x4c, 0xce, 0xc8, 0xcc, 0x4b, 0x2d, 0x56, 0xb2, 0x8a, 0x8e, 0xad, 0x05, 0x00, 0x09,
        0xff, 0x19, 0xd6, 0x0f, 0x00, 0x00, 0x00,
    ];

    async fn assert_gzip_fhcrc(wire: &[u8], expected_error: Option<io::ErrorKind>) {
        use tokio::{io::AsyncWriteExt, net::TcpListener};

        for image in [false, true] {
            for chunked in [false, true] {
                let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
                let address = listener.local_addr().unwrap();
                let mut response =
                    b"HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Encoding: gzip\r\n".to_vec();
                if chunked {
                    response.extend_from_slice(b"Transfer-Encoding: chunked\r\n\r\n");
                    for byte in wire {
                        response.extend_from_slice(b"1\r\n");
                        response.push(*byte);
                        response.extend_from_slice(b"\r\n");
                    }
                    response.extend_from_slice(b"0\r\n\r\n");
                } else {
                    response.extend_from_slice(
                        format!("Content-Length: {}\r\n\r\n", wire.len()).as_bytes(),
                    );
                    response.extend_from_slice(wire);
                }
                let server = tokio::spawn(async move {
                    tokio::time::timeout(Duration::from_secs(3), async {
                        let (mut socket, _) = listener.accept().await.unwrap();
                        let mut request = Vec::new();
                        while !request.ends_with(b"\r\n\r\n") {
                            assert!(request.len() < 65536);
                            request.push(socket.read_u8().await.unwrap());
                        }
                        assert!(
                            request
                                .windows(ACCEPTED_ENCODING.len())
                                .any(|value| value == ACCEPTED_ENCODING.as_bytes())
                        );
                        socket.write_all(&response).await.unwrap();
                    })
                    .await
                    .unwrap();
                });
                let http = client_builder().no_proxy().build().unwrap();
                let url = format!("http://{address}/image");
                let mut output = Vec::new();
                let outcome = tokio::time::timeout(Duration::from_secs(3), async {
                    if image {
                        let path = download(&http, &url, &mut output).await?;
                        let bytes = std::fs::read(&path);
                        path.close()?;
                        bytes
                    } else {
                        serde_json::to_vec(&index(&http, &url).await?).map_err(io::Error::other)
                    }
                })
                .await;
                server.await.unwrap();
                let result = outcome.expect("finite loopback codec request");
                match expected_error {
                    None => assert_eq!(result.unwrap(), br#"{"machines":[]}"#),
                    Some(kind) => {
                        let error = result.unwrap_err();
                        let primary = if image {
                            let transfer = error
                                .get_ref()
                                .unwrap()
                                .downcast_ref::<TransferFailure>()
                                .unwrap();
                            assert!(transfer.cleanup.is_empty());
                            &transfer.primary
                        } else {
                            &error
                        };
                        assert_eq!(primary.kind(), kind, "{primary}");
                    }
                }
                if image {
                    let output = String::from_utf8(output).unwrap();
                    let (_, path) = output.lines().next().unwrap().split_once(" -> ").unwrap();
                    assert!(!Path::new(path).exists(), "download temporary file removed");
                }
            }
        }
    }

    #[tokio::test]
    async fn gzip_fhcrc_valid_header_decodes_index_and_image() {
        assert_gzip_fhcrc(GZIP_FHCRC, None).await;
    }

    #[tokio::test]
    async fn gzip_fhcrc_invalid_header_retains_error_and_cleans_image() {
        let mut wire = GZIP_FHCRC.to_vec();
        wire[10] ^= 1;
        assert_gzip_fhcrc(&wire, Some(io::ErrorKind::InvalidData)).await;
    }

    #[tokio::test]
    async fn gzip_fhcrc_truncated_header_retains_eof_and_cleans_image() {
        for end in [10, 11] {
            assert_gzip_fhcrc(&GZIP_FHCRC[..end], Some(io::ErrorKind::UnexpectedEof)).await;
        }
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
        assert_eq!(
            generation_input("-2", generations).unwrap()["generation"],
            6
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
        let body = format!("#!/bin/sh\nprintf '%s\\000' \"$@\" > {record_quote}\nexit 0\n");
        // A concurrent fork must not inherit a writable fixture descriptor from
        // this multithreaded test process. The waited child owns that writer.
        let status = Command::new("/bin/sh")
            .args(["-c", "set -C; printf '%s' \"$2\" > \"$1\"", "write-fixture"])
            .arg(&executable)
            .arg(&body)
            .status()
            .unwrap();
        assert!(status.success(), "fixture writer failed: {status}");
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
