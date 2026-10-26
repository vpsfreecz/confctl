# Rust package and helpers

`packages.<system>.confctl` and `packages.<system>.confctl-rust` select the same
Rust package for x86_64-linux and aarch64-linux. The existing
`apps.<system>.confctl-rust` runs its CLI. The package provides both `confctl` and
`confctl-rust` in `bin`. Selecting a package does not change an installed
configuration or a flake pin.

Configuration shells use Rust by default:

```nix
confctl.lib.mkConfigDevShell {
  system = "x86_64-linux";
  mode = "minimal"; # use "tools" for repository Gemfile tooling
}
```

Minimal mode needs no Gemfile. Tools mode retains the configuration repository's
Ruby, OpenSSL, Bundler and Gemfile tooling, with the packaged confctl ahead of
those tools on PATH. Those gems are not loaded into confctl. Explicit
`implementation = "ruby"`, `mode = "bundled-confctl"` or `legacyCompat = true`
fails with migration instructions before Bundler bootstrap. Omitted or false
`legacyCompat` has no effect. An explicit `implementation = "rust"` remains valid.
MANPATH uses the package's installed manuals.

`mkConfctlOutputs` selects Rust helpers by default:

```nix
confctl.lib.mkConfctlOutputs {
  confDir = ./.;
  inherit inputs channels;
}
```

`rustHelpers = true` remains valid; explicit false fails with migration
instructions. A private module argument supplies the package built with each
evaluated target's pkgs, for both NixOS and vpsAdminOS. Standalone module imports
without that argument use the same-source Rust recipe with their target pkgs.
Auto rollback uses those target pkgs too. Machine-specific wrappers and JSON
reuse the same Rust derivation for the same source and target pkgs; FQDNs are not
compilation inputs. Existing enable options still control helper installation.
A target lacking a suitable Rust platform requires a separate input decision;
these selectors do not update inputs.

The package compiles the CLI and four helpers with the existing Cargo.lock and
default features. Ordinary public packages and target helper packages have
`doCheck = false`: installing them does not run the test suite. The lazy
`passthru.tests.qualification` derivation uses the same recipe, source, Cargo.lock
and target packages with `doCheck = true`. CI and release checks build that
derivation explicitly; runtime, helper, shell and smoke outputs do not depend on it.
A cache miss can still require compiling the release binaries and their
dependencies.

`CONFCTL_LOGNAME` is the lexical coreutils `bin/logname` entry
at compile time. The five backends live in `libexec/confctl`. The CLI wrapper
executes its absolute backend, retaining argv, stdin, working directory and
native exit status. It adds Nix, Git, OpenSSH, coreutils, Bash and less to PATH.
It supplies the packaged CA bundle only when the corresponding SSL_CERT_FILE or
NIX_SSL_CERT_FILE variable is unset; user proxy, CA, editor and pager settings
remain available. ClusterSSH environments and configured health commands are
resolved by their owning routes, not guessed package dependencies.

Module wrappers overwrite the four fixed private fields with literal package
data and use `exec -a "$0"` with unchanged arguments. Carrier keeps its generated
callback and native nix-env; kexec receives the absolute target kexec executable.
See [remote helpers](remote-helpers.md) for field meanings, effect ordering and
failure behavior. The netboot producer omits the unused `ruby` JSON key. Its
backend accepts both old and stripped JSON. Rollback to Ruby must
restore the old producer and wrapper together; it does not undo prior operational
effects.

Only the selected manuals, project license and vendored float-conversion notice
are installed with the binaries and wrappers. Pandoc renders checked-in Markdown
with explicit man-section/title metadata and no generated date. This includes
the CLI reference and kexec manual. Rust kexec module wrappers copy the manual
from the same Rust package as their backend. Pandoc is a build-only input.
The package does not install source trees, Ruby application code, test fixtures
or oracle archives. The application, helper producers, development shell and
release procedure use Rust tooling. Ruby remains in native test infrastructure,
immutable compatibility captures and independent configuration-repository tools.
See [compatibility](compatibility.md) for extension and state migration limits.

## Reference maintenance

The `generate-reference` Cargo example has two modes. `cli` reads the actual
command schema and public parser help; `options FILE` reads an explicit JSON
array exported by this repository's `#moduleOptions`. Neither mode runs child
commands or reads a configuration. The example is not installed as a CLI tool.

Generate a complete temporary candidate before replacing the CLI page:

```sh
candidate="$(mktemp man/man8/.confctl-reference.XXXXXX)"
cargo run --locked --example generate-reference -- cli > "$candidate" &&
  mv -- "$candidate" man/man8/confctl-reference.8.md
```

The page uses public help text, omitting line-end spaces, and adds option
spellings, aliases, negative forms and switch defaults from the schema. Defaults
absent from the option schema are not inferred from settings or help prose. The
[command manual](../man/man8/confctl.8.md) remains the explanation of command
behavior. Check the page and renderer without Nix:

```sh
cargo test --locked --example generate-reference
cargo run --locked --example generate-reference -- cli --check man/man8/confctl-reference.8.md
```

`--check FILE` compares the fully rendered output with a file and never writes
it. Generation metadata uses the Cargo package version and no wall-clock date.
The small `tests/fixtures/reference-options.json` is synthetic renderer data,
not an evaluated options inventory. It must never replace
`man/man8/confctl-options.nix.8.md`.

To refresh the options manual, export `#moduleOptions` from this repository using
its locked inputs. A site's `#confctl.moduleOptions` is a different export.

```sh
nix eval --no-update-lock-file --no-write-lock-file \
  --option allow-import-from-derivation false --option accept-flake-config false \
  --json .#moduleOptions > module-options.json
```

Render the export into a temporary candidate before replacing the manual:

```sh
candidate="$(mktemp man/man8/.confctl-options.XXXXXX)"
cargo run --locked --example generate-reference -- options module-options.json > "$candidate" &&
  mv -- "$candidate" man/man8/confctl-options.nix.8.md
```

The renderer keeps confctl settings, cluster metadata and system settings in
their existing groups. Descriptions retain literal/option markup and Unicode;
defaults and examples distinguish absent values, JSON null, false, ordinary
JSON data and explicit Nix `literalExpression` text. Unsupported expression or
field shapes fail before any output is written. Declaration paths from the
repository or its store source use stable `<confctl/nix/modules/...>` labels.
Package builds consume the checked-in Markdown; they do not run this example,
Nix, Ruby or compatibility oracles to regenerate it.

Renderer tests use synthetic data. Validate the full options inventory against
an actual export, and check the rendered manuals in the built package.

The package's `passthru.tests.smoke` prepares installed help, executable and
manual checks for later native execution. `passthru.tests.shellSelection` checks
Rust defaults, minimal/tools selection and rejected legacy combinations
against the shell function when evaluated. Cargo helper tests exercise actual
binaries with confined fake native commands, distinct callbacks, literal native
paths, empty FQDN, help-before-data and native failure. They do not evaluate Nix
wrappers or prove the installed closure. Runtime references, dependency notices,
linked libraries, both architectures and real shell/helper selections still need
native qualification.

The hosted Rust workflow uses Rust 1.95.0 with rustfmt and Clippy, verifies fixture
prerequisites and sets the lexical GNU logname entry before compiling. It checks
all targets/features and lints them, then runs default tests with qualification
mains disabled and ignored native tests left ignored. The example step tests
the reference renderer and checks CLI-page drift. Toolchain and crate downloads
occur in disposable hosted CI. Hosted checks do not establish pinned production-coreutils parity, evaluated
packages, installed manuals, real Nix builds, site migration or operator
acceptance. The self-hosted native suite remains separate.

A separate hosted job qualifies the package on native x86_64 and ARM Linux
runners. It checks that the public package and Rust alias select the same
runtime derivation, builds `tests.qualification` for the default tests, then builds
the runtime package and its smoke/shell-selection checks and executes both
installed CLI aliases. It also checks installed manuals,
license files and basic runtime-closure contents. These checks do not establish
VM, real-Nix source-inclusion, mixed-helper or operator acceptance.

## Stable input and binary cache

Confctl pins its own `nixos-26.05` input for public packages and development
shells. The VM framework keeps its separate vpsAdminOS input and fixture package
set. Helpers continue to use the evaluated target's packages. Independent locks
can select different revisions; cache reuse depends on the exact source, package
set, system and derivation, rather than the channel name alone.

The confctl repository's `cache.yml` builds and qualifies committed branch
revisions on the existing x86_64 `vpsadminos runners` group. It uploads only the
ordinary runtime closure to `cache.int.vpsadminos.org`, registers a flat
`confctl-<branch SHA256>-x86_64-linux` profile through `push-ci-generation`, and
checks the public cache signature. Existing server retention keeps these branch
generations for 90 days. Hosted native ARM qualification remains separate; this
workflow does not publish an ARM cache.

The repository must have access to that runner group and the existing
`VPSADMINOS_CACHE_SSH_KEY` secret. A missing secret fails on a hosted runner before
allocating a private runner. The upload job writes the pinned cache host key to
its own temporary `known_hosts` file and uses strict host verification for every
SSH step, independently of the runner's global trust files. It fails before
building if host verification or authentication fails.
It never processes pull-request events or fork credentials. Cache publication
requires successful explicit qualification and installed checks.

`update-nixpkgs.yml` runs at 06:45 UTC. It updates only the direct stable input,
rejects unrelated lock changes, and uses normal repository hooks for a minimal
lock commit. A changed remote branch stops publication; there is no force push.
The same workflow directly invokes the shared cache job for its resulting commit,
including when the lock did not change. It does not depend on a bot push starting
another workflow. These schedules take effect when present on the default branch.

Clients need `https://cache.vpsadminos.org` in their Nix substituters and the public
trusted key
`cache.vpsadminos.org:wpIJlNZQIhS+0gFf1U3MC9sLZdLW3sh5qakOWGDoDrE=`.
The package does not change a client's trust settings. Public metadata and a valid
signature establish publication; a cache-hit acceptance check additionally
substitutes the exact runtime output into a disposable store without build fallback.
An already-built local output does not establish a cache hit.

The self-hosted native VM/GC job runs automatically only for default-branch
pushes matching its path filters. Feature branches and tags require deliberate
`workflow_dispatch` with separate runner ownership and authorization. The guard skips the whole job before runner
allocation, including GC reset and cleanup; a skipped job is not a native
test pass. Older refs retain their existing scheduling until they adopt the
guard. The hosted Rust workflow runs independently under its own triggers.
