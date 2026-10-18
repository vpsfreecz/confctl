//! Closed command schema; aliases are compatibility inputs, not operation maps.

#[derive(Clone, Copy)]
pub enum Kind {
    Switch(bool),
    Value,
    Repeat,
    Integer,
}
#[derive(Clone, Copy)]
pub struct OptionSpec {
    pub name: &'static str,
    pub aliases: &'static [&'static str],
    pub kind: Kind,
    pub negatable: bool,
}

const fn switch(name: &'static str, aliases: &'static [&'static str], default: bool) -> OptionSpec {
    OptionSpec {
        name,
        aliases,
        kind: Kind::Switch(default),
        negatable: true,
    }
}
const fn plain(name: &'static str, aliases: &'static [&'static str]) -> OptionSpec {
    OptionSpec {
        name,
        aliases,
        kind: Kind::Switch(false),
        negatable: false,
    }
}
const fn value(name: &'static str, aliases: &'static [&'static str]) -> OptionSpec {
    OptionSpec {
        name,
        aliases,
        kind: Kind::Value,
        negatable: false,
    }
}
const fn integer(name: &'static str, aliases: &'static [&'static str]) -> OptionSpec {
    OptionSpec {
        name,
        aliases,
        kind: Kind::Integer,
        negatable: false,
    }
}

pub const ROUTES: &[&str] = &[
    "init",
    "add",
    "rename",
    "rediscover",
    "inputs ls",
    "inputs update",
    "inputs set",
    "inputs channel ls",
    "inputs channel update",
    "inputs channel set",
    "inputs machine update",
    "inputs machine set",
    "ls",
    "build",
    "deploy",
    "health-check",
    "status",
    "changelog",
    "diff",
    "test-connection",
    "ssh",
    "cssh",
    "generation ls",
    "generation rm",
    "generation rotate",
    "collect-garbage",
    "gen-data vpsadmin all",
    "gen-data vpsadmin containers",
    "gen-data vpsadmin network",
    "runtime-kernels update",
];

pub fn children(path: &str) -> Vec<&'static str> {
    let mut names = Vec::new();
    for route in ROUTES {
        let tail = if path.is_empty() {
            Some(*route)
        } else {
            route.strip_prefix(path).and_then(|v| v.strip_prefix(' '))
        };
        if let Some(name) = tail.and_then(|s| s.split(' ').next())
            && !names.contains(&name)
        {
            names.push(name);
        }
    }
    names.sort_unstable();
    names
}

pub fn options(path: &str) -> Vec<OptionSpec> {
    let mut result = Vec::new();
    if matches!(
        path,
        "ls" | "build"
            | "deploy"
            | "health-check"
            | "status"
            | "changelog"
            | "diff"
            | "test-connection"
            | "ssh"
            | "cssh"
            | "generation ls"
            | "generation rm"
            | "generation rotate"
            | "collect-garbage"
            | "runtime-kernels update"
    ) {
        result.extend([
            OptionSpec {
                name: "attr",
                aliases: &["a"],
                kind: Kind::Repeat,
                negatable: false,
            },
            OptionSpec {
                name: "tag",
                aliases: &["t"],
                kind: Kind::Repeat,
                negatable: false,
            },
        ]);
    }
    if matches!(
        path,
        "build"
            | "deploy"
            | "health-check"
            | "status"
            | "changelog"
            | "diff"
            | "test-connection"
            | "ssh"
            | "cssh"
            | "generation rm"
            | "generation rotate"
            | "collect-garbage"
            | "runtime-kernels update"
    ) {
        result.push(switch("yes", &["y"], false));
    }
    if matches!(path, "ls" | "test-connection" | "ssh" | "cssh") {
        result.push(value("managed", &[]));
    }
    if matches!(path, "ls" | "build" | "deploy" | "runtime-kernels update") {
        result.push(switch("show-trace", &[], false));
    }
    if matches!(path, "build" | "deploy" | "changelog" | "diff") {
        result.extend([value("max-jobs", &["j"]), value("cores", &[])]);
    }
    if matches!(path, "deploy" | "status" | "changelog" | "diff") {
        result.push(value("generation", &["g"]));
    }
    if matches!(
        path,
        "inputs update"
            | "inputs set"
            | "inputs channel update"
            | "inputs channel set"
            | "inputs machine update"
            | "inputs machine set"
    ) {
        result.extend([
            switch("commit", &[], false),
            switch("changelog", &[], true),
            switch("editor", &[], true),
            switch("downgrade", &["d"], false),
        ]);
    }
    if matches!(
        path,
        "generation ls" | "generation rm" | "generation rotate"
    ) {
        result.extend([
            switch("local", &["l"], false),
            switch("remote", &["r"], false),
        ]);
    }
    if matches!(path, "generation rm" | "generation rotate") {
        result.push(switch("collect-garbage", &["gc"], true));
    }
    if matches!(
        path,
        "generation rm" | "generation rotate" | "collect-garbage"
    ) {
        result.push(integer("max-concurrent-gc", &[]));
    }
    match path {
        "inputs update" => result.push(switch("all", &[], false)),
        "inputs channel set" => result.push(switch("allow_shared", &["allow-shared"], false)),
        "ls" => result.extend([
            plain("list", &["L"]),
            value("output", &["o"]),
            switch("hide-header", &["H"], false),
        ]),
        "deploy" => result.extend([
            switch("interactive", &["i"], false),
            switch("dry-activate-first", &[], false),
            switch("one-by-one", &[], false),
            integer("max-concurrent-copy", &[]),
            plain("copy-only", &[]),
            plain("enable-auto-rollback", &[]),
            plain("disable-auto-rollback", &[]),
            switch("reboot", &[], false),
            value("wait-online", &[]),
            switch("health-checks", &[], true),
            switch("keep-going", &[], false),
        ]),
        "health-check" => result.push(integer("max-jobs", &["j"])),
        "changelog" => result.extend([
            switch("downgrade", &["d"], false),
            switch("verbose", &["v"], false),
            switch("patch", &["p"], false),
        ]),
        "diff" => result.push(switch("downgrade", &["d"], false)),
        "ssh" => result.extend([
            switch("parallel", &["p"], false),
            switch("aggregate", &["g"], false),
            value("input-string", &["i"]),
            value("input-file", &["f"]),
        ]),
        _ => {}
    }
    result
}

pub fn arity(path: &str) -> Option<(&'static [&'static str], usize)> {
    match path {
        "add" => Some((&["name"], 0)),
        "rename" => Some((&["old-name", "new-name"], 0)),
        "inputs set" => Some((&["input-name", "rev"], 0)),
        "inputs channel update" => Some((&["channels"], 1)),
        "inputs channel set" => Some((&["channels", "role", "rev"], 0)),
        "inputs machine update" => Some((&["machine", "role"], 0)),
        "inputs machine set" => Some((&["machine", "role", "rev"], 0)),
        _ => None,
    }
}
