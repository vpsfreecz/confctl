use super::*;
#[test]
fn included_associations_preserve_data_and_unresolved_path_parameters() {
    let association: Association = serde_json::from_value(json!({
        "id":7,"name":"node","domain_name":"node.test",
        "location":{"domain":"test","_meta":{"resolved":true}},
        "_meta":{"resolved":true}}))
    .unwrap();
    assert!(resolved(&association));
    let node: resources::node::show::Output = included(&association).unwrap();
    assert_eq!(node.id, Some(7));
    assert_eq!(node.name.as_deref(), Some("node"));
    assert!(resolved(node.location.as_ref().unwrap()));
    let unresolved: Association = serde_json::from_value(json!({
        "_meta":{"resolved":false,"path_params":["node/7"]}}))
    .unwrap();
    assert!(!resolved(&unresolved));
    assert_eq!(association_id(&unresolved).unwrap(), "node/7");
    for value in [
        json!({}),
        json!({"_meta":{"path_params":[]}}),
        json!({"_meta":{"path_params":[1,2]}}),
        json!({"_meta":{"path_params":[{}]}}),
    ] {
        let association: Association = serde_json::from_value(value).unwrap();
        assert!(association_id(&association).is_err());
    }
}
#[test]
fn fixture_endpoints_remain_numeric_loopback_only() {
    for endpoint in [
        "http://localhost:1/",
        "http://192.0.2.1:1/",
        "https://127.0.0.1:1/",
        "http://127.0.0.1:1/?x",
        "http://user@127.0.0.1:1/",
        "http://127.0.0.1:1/#fragment",
    ] {
        assert!(test_base(endpoint).is_err());
    }
    assert!(test_base("http://127.0.0.1:1/").is_ok());
    assert!(test_base("http://[::1]:1/").is_ok());
}
