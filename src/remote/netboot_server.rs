//! Real carrier netboot publisher: scan Nix profiles under the existing lock,
//! resolve current metadata and all generations, build complete HTTP/TFTP trees,
//! then install each root in the original order. The pair is not a transaction:
//! an HTTP install failure can follow a successful TFTP install. Old roots are
//! cleaned only after both installs succeed, preserving the original boundary.

mod model;
mod render;

include!("netboot_server/common.rs");

pub fn entry() -> i32 {
    common_entry(
        &BuilderPaths {
            profiles: Path::new("/nix/var/nix/profiles"),
            lock: Path::new("/run/confctl/build-netboot-server.lock"),
            entropy: Path::new("/dev/urandom"),
        },
        &mut io::stdout(),
        &mut io::stderr(),
    )
}

#[cfg(test)]
mod tests;
