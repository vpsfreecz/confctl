//! Explicit record-only qualification entry. It is ignored by ordinary suites
//! and has no accepted output table. It exercises the real private adapter.

#[tokio::test]
#[ignore = "requires one owned loopback case and receipt path from the qualification driver"]
async fn native_http_qualification_records_one_case_without_promoting_expectations() {
    use super::*;
    use serde_json::json;

    let url = std::env::var("CONFCTL_TEST_HTTP_URL").expect("owned loopback URL");
    let receipt = std::env::var_os("CONFCTL_TEST_HTTP_RECEIPT").expect("owned receipt path");
    let mode = std::env::var("CONFCTL_TEST_HTTP_MODE").expect("body or index mode");
    let uri = reqwest::Url::parse(&url).unwrap();
    assert!(matches!(uri.host_str(), Some("127.0.0.1" | "localhost")));
    let client = super::super::client_builder().no_proxy().build().unwrap();
    let mut decoded = Vec::new();
    let mut trailing = None;
    let outcome = async {
        let response = super::super::get(&client, &url).await?;
        let mut body = Body::new(response).await?;
        let mut buffer = [0u8; 4096];
        loop {
            let count = body.read(&mut buffer).await?;
            if count == 0 {
                break;
            }
            decoded.extend_from_slice(&buffer[..count]);
            if decoded.len() > 1024 * 1024 {
                return Err(io::Error::other(
                    "qualification fixture exceeds its 1MiB bound",
                ));
            }
        }
        trailing = Some(body.finish_transport().await?);
        if mode == "index" {
            super::super::parse_index(&decoded)?;
        } else {
            assert_eq!(mode, "body");
        }
        Ok::<(), io::Error>(())
    }
    .await;
    let error = outcome.as_ref().err();
    let coordinates = error
        .and_then(|error| error.get_ref())
        .and_then(|error| error.downcast_ref::<super::super::IndexJsonFailure>())
        .map(|error| json!({"line":error.source.line(),"column":error.source.column()}));
    let result = json!({
        "scope":"private adapter record only; not a full helper process/parity assertion",
        "decoded_bytes":decoded,
        "success":outcome.is_ok(),
        "error_kind":error.map(|error| format!("{:?}",error.kind())),
        "error_primary":error.map(ToString::to_string),
        "parser_coordinates":coordinates,
        "post_codec_raw_bytes":trailing,
        "policy_gap":"trailing/member/empty decoder semantics require the retained original/candidate table"
    });
    std::fs::write(receipt, serde_json::to_vec_pretty(&result).unwrap()).unwrap();
}
