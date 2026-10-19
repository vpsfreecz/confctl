//! Fixture-private bridge; resolver construction stays at common client point.
#[path = "../../../../../src/remote/kexec_netboot/body.rs"]
mod body;
include!("../../../../../src/remote/kexec_netboot/common.rs");

pub(super) fn invoke(
    root: &Path,
    destination: std::net::SocketAddrV4,
    stdout: &mut impl Write,
    stderr: &mut impl Write,
) -> i32 {
    let cmdline = root.join("cmdline");
    common_entry(
        OsStr::new("kexec-netboot"),
        &[],
        &KexecInputs {
            cmdline: &cmdline,
            fixture_destination: Some(destination),
        },
        &mut io::stdin(),
        stdout,
        stderr,
    )
}
