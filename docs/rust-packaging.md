# Opt-in Rust package and helpers

`packages.<system>.confctl-rust` and `apps.<system>.confctl-rust` select the
Rust CLI for x86_64-linux and aarch64-linux. The existing `confctl` package and
Ruby defaults remain available. Selecting the Rust package exposes both
`confctl` and `confctl-rust` in that package's `bin`; it does not change an
installed configuration or a flake pin.

For a configuration shell, select the implementation explicitly:

```nix
confctl.lib.mkConfigDevShell {
  system = "x86_64-linux";
  implementation = "rust";
  mode = "minimal"; # use "tools" for repository Gemfile tooling
}
```

Rust supports minimal and tools modes. Repository gems in tools mode belong to
those tools; the packaged CLI does not load them. Rust with bundled-confctl
fails during evaluation, before Bundler bootstrap, with instructions to use
tools mode. Unknown implementation values also fail. Ruby modes and
`legacyCompat` retain their existing behavior. Rust MANPATH uses the package's
installed manuals.

Helper selection is independent of CLI selection:

```nix
confctl.lib.mkConfctlOutputs {
  confDir = ./.;
  inherit inputs channels;
  rustHelpers = true;
}
```

`rustHelpers` defaults to false. When enabled, a private module argument supplies
the package built with each evaluated target's pkgs, for both NixOS and
vpsAdminOS. Auto rollback uses those target pkgs too. Machine-specific wrappers
and JSON reuse the same Rust derivation for the same source and target pkgs;
FQDNs are not compilation inputs. Standalone module imports without that private
argument keep their Ruby producers. Existing enable options still control which
helpers are installed. A target lacking a suitable Rust platform must be updated
through a separate input decision; this option does not change inputs.

The package compiles the CLI and four helpers with the existing Cargo.lock and
default features. `CONFCTL_LOGNAME` is the lexical coreutils `bin/logname` entry
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
failure behavior. The Rust netboot producer omits only the unused `ruby` JSON
key. Its backend accepts both old and stripped JSON. Rollback to Ruby must
restore the old producer and wrapper together; it does not undo prior operational
effects.

Only the selected manuals, project license and vendored float-conversion notice
are installed with the binaries and wrappers. Pandoc renders checked-in Markdown
with explicit man-section/title metadata and no generated date. This includes
the CLI reference and kexec manual. Rust kexec module wrappers copy the manual
from the same Rust package as their backend. Pandoc is a build-only input.
The package does not install source trees, Ruby application code, test fixtures
or oracle archives. Ruby defaults, its AsciiDoc manual branch, Rake/ERB generation
and release tools remain available during coexistence.

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

To refresh the options manual, export `#moduleOptions` from this repository's
locked inputs. This command uses locally available inputs and fails if an input
is missing. A site's `#confctl.moduleOptions` is a different export.

```sh
nix eval --offline --no-update-lock-file --no-write-lock-file \
  --option allow-import-from-derivation false --json .#moduleOptions > module-options.json
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
Ruby defaults, explicit Rust minimal/tools selection and rejected combinations
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
occur in disposable hosted
CI. Hosted checks do not establish pinned production-coreutils parity, evaluated
packages, installed manuals, real Nix builds, site migration or operator
acceptance. Ruby workflows and the self-hosted suite remain separate.

The self-hosted native VM/GC job runs automatically only for default-branch
pushes matching its existing path filters. Feature branches, including Ruby
changes, and tags require deliberate `workflow_dispatch` with separate runner
ownership and authorization. The guard skips the whole job before runner
allocation, including GC reset and cleanup; a skipped job is not a native
test pass. Older refs retain their existing scheduling until they adopt the
guard. Hosted Rust, RuboCop and RSpec workflows keep their existing triggers.
