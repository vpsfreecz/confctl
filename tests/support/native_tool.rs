// Resolve before a fixture confines PATH. Keep the lexical applet entry: resolving
// a coreutils symlink to its multicall target changes argv0 and its behavior.
use std::{os::unix::fs::PermissionsExt, path::PathBuf};

pub fn resolve(name: &str) -> PathBuf {
    let search = std::env::var_os("PATH").expect("native test tools require the original PATH");
    let cwd = std::env::current_dir().expect("native test tool lookup cwd");
    std::env::split_paths(&search)
        .map(|directory| {
            if directory.is_absolute() {
                directory
            } else {
                cwd.join(directory)
            }
        })
        .map(|directory| directory.join(name))
        .find(|path| {
            std::fs::metadata(path).is_ok_and(|metadata| {
                metadata.is_file() && metadata.permissions().mode() & 0o111 != 0
            })
        })
        .unwrap_or_else(|| {
            panic!("required native test executable {name} not found on original PATH")
        })
}
