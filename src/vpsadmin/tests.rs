use super::*;
#[test]
fn described_urls_are_confined_before_credentials_or_tokens() {
    let runtime = tokio::runtime::Builder::new_current_thread()
        .enable_all()
        .build()
        .unwrap();
    runtime.block_on(async {
        let (events, _receiver) = EventSink::channel(1);
        let cancel = CancellationToken::new();
        let ui = Ui::new(
            std::fs::File::open("/dev/null").unwrap(),
            events.clone(),
            cancel.clone(),
            None,
        )
        .unwrap();
        let client = Client::loopback(
            &ui,
            &events,
            &cancel,
            "http://127.0.0.1:1/api/",
            std::sync::Arc::new(std::sync::atomic::AtomicI64::new(0)),
        )
        .unwrap();
        for path in [
            "http://127.0.0.1:2/api/",
            "http://user@127.0.0.1:1/api/",
            "//127.0.0.1:1/api/",
            "/outside",
            "../outside",
            "%2e%2e/outside",
            "../api/inside",
            "/api/{id}",
            "/api/foo#fragment",
        ] {
            assert!(
                client.action_url(&json!({"path":path}), &[]).is_err(),
                "{path}"
            );
        }
        assert_eq!(
            client
                .action_url(&json!({"path":"/api/vps/{id}"}), &[json!("a/b ?é")])
                .unwrap()
                .as_str(),
            "http://127.0.0.1:1/api/vps/a%2Fb%20%3F%C3%A9"
        );
        for endpoint in [
            "http://localhost:1/",
            "http://192.0.2.1:1/",
            "https://127.0.0.1:1/",
            "http://127.0.0.1:1/?x",
        ] {
            assert!(test_base(endpoint).is_err());
        }
        ui.shutdown().unwrap();
    });
}
