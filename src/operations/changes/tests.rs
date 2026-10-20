use super::*;
use serde_json::json;

#[test]
fn role_union_and_url_fallback_preserve_sorted_roles_and_ruby_truthiness() {
    let target = input_observation::normalize(&json!({"z":{"rev":"a"},"a":{"rev":false}})).unwrap();
    let deployed =
        input_observation::normalize(&json!({"b":{"rev":7},"a":{"rev":"false"}})).unwrap();
    assert_eq!(
        roles(Some(&target), Some(&deployed))
            .into_iter()
            .collect::<Vec<_>>(),
        ["a", "b", "z"]
    );
    let deployed = json!({"url":"deployed"});
    for (value, expected) in [
        (json!(false), json!("deployed")),
        (Value::Null, json!("deployed")),
        (json!(""), json!("")),
        (json!(7), json!(7)),
    ] {
        assert_eq!(
            chosen_url(Some(&json!({"url":value})), Some(&deployed)),
            Some(&expected)
        );
    }
    assert_eq!(
        short(Some(&json!({"rev":"abcdefgh-full","shortRev":"custom"}))),
        "custom"
    );
    assert_eq!(
        input_observation::comparison(target.get("a"), Some(&json!({"rev":false}))),
        Comparison::Same
    );
}
