//! Fixture-private bridge; no preloaded config, suffix or entropy read.
#[path = "../../../../../src/remote/netboot_server/model.rs"]
mod model;
#[path = "../../../../../src/remote/netboot_server/render.rs"]
mod render;
include!("../../../../../src/remote/netboot_server/common.rs");

pub(super) fn invoke(root: &Path, stdout: &mut impl Write, stderr: &mut impl Write) -> i32 {
    let profiles = root.join("profiles");
    let lock = root.join("run/confctl/build-netboot-server.lock");
    let entropy = root.join("entropy");
    common_entry(
        &BuilderPaths {
            profiles: &profiles,
            lock: &lock,
            entropy: &entropy,
        },
        stdout,
        stderr,
    )
}
