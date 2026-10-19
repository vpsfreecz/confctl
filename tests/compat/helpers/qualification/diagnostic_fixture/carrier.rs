//! Fixture-private bridge; the same helper body is included by the product.
include!("../../../../../src/remote/carrier_env/common.rs");

pub(super) fn fixture_entry(
    root: &Path,
    short_profile: bool,
    stdout: &mut impl Write,
    stderr: &mut impl Write,
) -> i32 {
    let flag = if short_profile { "-p" } else { "--profile" };
    let args = vec![
        OsString::from(flag),
        root.join("profile").into_os_string(),
        OsString::from("--set"),
        OsString::from("/nix/store/fixture"),
    ];
    common_entry(OsStr::new("carrier-env"), &args, stdout, stderr)
}
