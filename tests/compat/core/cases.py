"""Declarative Ruby CLI oracle cases; all external effects use child.py."""

BASELINE = "cc40679d267165aecfa569128438bb55fe910268"

# Flags are retained literally from the Ruby registration, including allow_shared.
ROUTES = [
    ("init", "", []),
    ("add", "name", []),
    ("rename", "old-name new-name", []),
    ("rediscover", "", []),
    ("inputs ls", "[input-pattern]", []),
    ("inputs update", "[input-name ...]", ["I", "all=false"]),
    ("inputs set", "input-name rev", ["I"]),
    ("inputs channel ls", "[channel-pattern]", []),
    ("inputs channel update", "channels [role]", ["I"]),
    ("inputs channel set", "channels role rev", ["I", "allow_shared=false"]),
    ("inputs machine update", "machine role", ["I"]),
    ("inputs machine set", "machine role rev", ["I"]),
    ("ls", "[machine-pattern]", ["F", "M", "show-trace", "L/list", "o/output", "H/hide-header"]),
    ("build", "[machine-pattern]", ["F", "Y", "N", "show-trace"]),
    ("deploy", "[machine-pattern [switch-action]]", ["F", "Y", "N", "show-trace", "g/generation", "i/interactive", "dry-activate-first", "one-by-one", "max-concurrent-copy=5:Integer", "copy-only", "enable-auto-rollback", "disable-auto-rollback", "reboot", "wait-online=600", "health-checks=true", "keep-going=false"]),
    ("health-check", "[machine-pattern]", ["F", "Y", "j/max-jobs=5:Integer"]),
    ("status", "[machine-pattern]", ["F", "Y", "g/generation"]),
    ("changelog", "[machine-pattern [role-pattern]]", ["F", "Y", "N", "g/generation", "d/downgrade", "v/verbose", "p/patch"]),
    ("diff", "[machine-pattern [role-pattern]]", ["F", "Y", "N", "g/generation", "d/downgrade"]),
    ("test-connection", "[machine-pattern]", ["F", "M", "Y"]),
    ("ssh", "[machine-pattern [command [arguments...]]]", ["F", "M", "Y", "p/parallel", "g/aggregate", "i/input-string", "f/input-file"]),
    ("cssh", "[machine-pattern]", ["F", "M", "Y"]),
    ("generation ls", "[machine-pattern [generation-pattern]]", ["F", "L/R"]),
    ("generation rm", "[machine-pattern [generation-pattern|old]]", ["F", "L/R", "GC", "Y"]),
    ("generation rotate", "[machine-pattern]", ["F", "L/R", "GC", "Y"]),
    ("collect-garbage", "[machine-pattern]", ["F", "Y", "max-concurrent-gc=5:Integer"]),
    ("gen-data vpsadmin all", "", []),
    ("gen-data vpsadmin containers", "", []),
    ("gen-data vpsadmin network", "", []),
]

OPTION_GROUPS = {
    "F": ["a/attr:repeatable", "t/tag:repeatable"],
    "M": ["managed=y|yes|n|no|a|all:default-managed"],
    "Y": ["y/yes=false"],
    "N": ["j/max-jobs:string", "cores:string"],
    "I": ["commit=false", "changelog=true", "editor=true", "d/downgrade=false"],
    "L/R": ["l/local=false", "r/remote=false", "neither-means-local"],
    "GC": ["gc/collect-garbage=true", "max-concurrent-gc=5:Integer"],
}


def case(name, argv, *, setup="cluster", stdin="", mode="normal", route=None):
    return dict(id=name, argv=argv, setup=setup, stdin=stdin, mode=mode, route=route)


def cases():
    result = [case("help-" + route.replace(" ", "-"), route.split() + ["--help"],
                   setup="empty", route=route) for route, _, _ in ROUTES]
    result += [
        case("root-help", ["--help"], setup="empty"),
        case("default-help", [], setup="empty"),
        case("help-completion", ["help", "-c"], setup="empty"),
        case("version", ["--version"], setup="empty"),
        case("unknown-command", ["unknown"], setup="empty"),
        case("ambiguous-prefix", ["c"], setup="empty"),
        case("unique-prefix", ["redi"]),
        case("leaf-short-help", ["build", "-h"], setup="empty"),
        case("invalid-color", ["--color", "rainbow", "ls"]),
        case("options-after-args", ["add", "alpha", "--yes"]),
        case("integer-invalid", ["deploy", "--max-concurrent-copy", "many"]),
        case("deploy-manual-only-outdated", ["deploy", "--outdated"]),
        case("channel-set-hyphen", ["inputs", "channel", "set", "--allow-shared", "base", "nixpkgs", "next"]),
        case("channel-set-underscore", ["inputs", "channel", "set", "--allow_shared", "base", "nixpkgs", "next"]),
        case("init", ["init"], setup="empty", route="init"),
        case("init-not-empty", ["init"]),
        case("add", ["add", "nested/new"], route="add"),
        case("add-missing", ["add"]),
        case("rename", ["rename", "alpha", "nested/renamed"], route="rename"),
        case("rediscover", ["rediscover"], route="rediscover"),
        case("ls-default", ["ls"], route="ls"),
        case("ls-all", ["ls", "--managed", "all"]),
        case("ls-filter", ["ls", "-H", "-o", "name,host.target", "-a", "spin=nixos", "-t", "prod", "a*"]),
        case("ls-negative-filters", ["ls", "--no-hide-header", "-a", "spin!=vpsadminos", "-t", "^other", "alpha-key"]),
        case("ls-list-attributes", ["ls", "-L"]),
        case("ls-surplus", ["ls", "alpha", "ignored"]),
        case("inputs-ls", ["inputs", "ls"], route="inputs ls"),
        case("inputs-update", ["inputs", "update", "nixpkgs"], route="inputs update"),
        case("inputs-update-all", ["inputs", "update", "--all"]),
        case("inputs-update-needs-name", ["inputs", "update"]),
        case("inputs-update-cached-warning", ["inputs", "update", "nixpkgs"], mode="cached-warning"),
        case("inputs-update-commit", ["inputs", "update", "--commit", "--no-changelog", "--no-editor", "nixpkgs"]),
        case("inputs-set", ["inputs", "set", "--no-commit", "--no-editor", "nixpkgs", "next"], route="inputs set"),
        case("channel-ls", ["inputs", "channel", "ls"], route="inputs channel ls"),
        case("channel-update", ["inputs", "channel", "update", "base", "nixpkgs"], route="inputs channel update"),
        case("channel-set-shared-refusal", ["inputs", "channel", "set", "base", "nixpkgs", "next"], route="inputs channel set"),
        case("machine-update", ["inputs", "machine", "update", "alpha", "nixpkgs"], route="inputs machine update"),
        case("machine-set", ["inputs", "machine", "set", "alpha", "nixpkgs", "next"], route="inputs machine set"),
        case("build-reuse", ["build", "--yes", "--show-trace", "--max-jobs", "2", "--cores", "3", "alpha"], route="build"),
        case("build-decline", ["build", "alpha"], stdin="n\n"),
        case("build-blank-then-decline", ["build", "alpha"], stdin="\nn\n"),
        case("build-eof", ["build", "alpha"]),
        case("build-no-yes", ["build", "--no-yes", "alpha"], stdin="n\n"),
        case("build-child-failure", ["build", "-y", "alpha"], mode="build-failure"),
        case("build-no-machine", ["build", "-y", "absent"]),
        case("deploy-switch", ["deploy", "-y", "-g", "current", "alpha"], route="deploy"),
        case("deploy-test", ["deploy", "-y", "-g", "current", "alpha", "test"]),
        case("deploy-dry", ["deploy", "-y", "-g", "current", "alpha", "dry-activate"]),
        case("deploy-boot", ["deploy", "-y", "-g", "current", "alpha", "boot"]),
        case("deploy-copy-only", ["deploy", "-y", "--copy-only", "-g", "current", "alpha"]),
        case("deploy-current-skip", ["deploy", "-y", "-g", "current", "alpha"], mode="current"),
        case("deploy-invalid-action", ["deploy", "-y", "alpha", "invalid"]),
        case("deploy-invalid-reboot", ["deploy", "--reboot", "alpha"]),
        case("deploy-invalid-wait", ["deploy", "--reboot", "--wait-online", "bad", "alpha", "boot"]),
        case("health-success", ["health-check", "-y", "alpha"], route="health-check"),
        case("status-current", ["status", "-y", "-g", "current", "alpha"], route="status"),
        case("status-none", ["status", "-y", "-g", "none", "alpha"]),
        case("status-offline", ["status", "-y", "-g", "none", "alpha"], mode="offline"),
        case("changelog-no-changes", ["changelog", "-y", "alpha"], route="changelog"),
        case("diff-no-changes", ["diff", "-y", "alpha"], route="diff"),
        case("connection-success", ["test-connection", "-y", "alpha"], route="test-connection"),
        case("connection-failed-host", ["test-connection", "-y", "alpha"], mode="offline"),
        case("ssh-success", ["ssh", "-y", "alpha", "printf", "%s", "space ; $literal"], route="ssh"),
        case("ssh-nonzero", ["ssh", "-y", "alpha", "false"], mode="ssh-failure"),
        case("ssh-aggregate", ["ssh", "-y", "--parallel", "--aggregate", "alpha", "printf", "same"]),
        case("ssh-input", ["ssh", "-y", "--input-string", "hello\n", "alpha", "cat"]),
        case("ssh-input-conflict", ["ssh", "--input-string", "data", "--input-file", "input.txt", "alpha", "cat"]),
        case("ssh-interactive", ["ssh", "-y", "alpha"]),
        case("cssh", ["cssh", "-y", "alpha"], route="cssh"),
        case("generation-list", ["generation", "ls", "alpha"], route="generation ls"),
        case("generation-numeric", ["generation", "ls", "alpha", "0"]),
        case("generation-invalid-current", ["generation", "ls", "alpha", "current"], mode="invalid-current"),
        case("generation-unsupported-numeric", ["generation", "ls", "alpha", "0"], mode="unsupported"),
        case("generation-rm-old-empty", ["generation", "rm", "-y", "alpha", "old"], route="generation rm"),
        case("generation-remote-rm", ["generation", "rm", "-y", "--remote", "--no-gc", "alpha", "old"]),
        case("generation-rotate-empty", ["generation", "rotate", "-y", "alpha"], route="generation rotate"),
        case("gc-baseline-line-error", ["collect-garbage", "-y", "alpha"], route="collect-garbage"),
        case("gc-child-failure", ["collect-garbage", "-y", "alpha"], mode="gc-failure"),
        case("gen-data-all", ["gen-data", "vpsadmin", "all"], mode="api", stdin="fixture-user\nfixture-password\n123456\n", route="gen-data vpsadmin all"),
        case("gen-data-containers", ["gen-data", "vpsadmin", "containers"], mode="api", stdin="fixture-user\nfixture-password\n123456\n", route="gen-data vpsadmin containers"),
        case("gen-data-network", ["gen-data", "vpsadmin", "network"], mode="api", stdin="fixture-user\nfixture-password\n123456\n", route="gen-data vpsadmin network"),
        case("gen-data-auth-error", ["gen-data", "vpsadmin", "network"], mode="api-auth-error", stdin="fixture-user\nfixture-password\n"),
        case("gen-data-redirect-blocked", ["gen-data", "vpsadmin", "network"], mode="api-redirect", stdin="fixture-user\nfixture-password\n123456\n"),
        case("gen-data-http-guard", ["--help"], mode="api-guard-http"),
        case("gen-data-socket-guard", ["--help"], mode="api-guard-socket"),
    ]
    flag_values = dict(attr="spin=nixos", tag="prod", managed="all", output="name",
                       **{"max-jobs": "2", "cores": "3", "generation": "current",
                          "max-concurrent-copy": "1", "wait-online": "nowait",
                          "input-string": "data", "input-file": "fixture.txt",
                          "max-concurrent-gc": "1"})
    nonnegative = {"list", "copy-only", "enable-auto-rollback", "disable-auto-rollback"}
    for route, _, options in ROUTES:
        for group in options:
            for specification in OPTION_GROUPS.get(group, [group]):
                if specification == "neither-means-local":
                    continue
                aliases = specification.split(":")[0].split("=")[0].split("/")
                name = aliases[-1]
                for alias in aliases:
                    option = ("-" if len(alias) == 1 else "--") + alias
                    args = route.split() + [option]
                    if name in flag_values:
                        args.append(flag_values[name])
                    result.append(case("option-" + route.replace(" ", "-") + "-" + alias,
                                       args + ["--help"], setup="empty", route=route))
                    if len(alias) > 1 and name not in flag_values and name not in nonnegative:
                        result.append(case("option-" + route.replace(" ", "-") + "-no-" + alias,
                                           route.split() + ["--no-" + alias, "--help"],
                                           setup="empty", route=route))
    return result
