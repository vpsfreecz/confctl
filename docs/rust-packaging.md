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
with explicit man-section/title metadata. The kexec manual uses Asciidoctor at
build time. Neither tool is a runtime dependency through the install recipe.
The package does not install source trees, Ruby application code, test fixtures
or oracle archives. CLI/Nix-derived reference generation and final Ruby
build/release-tool removal remain later work.

The package's `passthru.tests.smoke` prepares installed help, executable and
manual checks for later native execution. `passthru.tests.shellSelection` checks
Ruby defaults, explicit Rust minimal/tools selection and rejected combinations
against the shell function when evaluated. Cargo helper tests exercise actual
binaries with confined fake native commands, distinct callbacks, literal native
paths, empty FQDN, help-before-data and native failure. They do not evaluate Nix
wrappers or prove the installed closure. Runtime references, dependency notices,
linked libraries, both architectures and real shell/helper selections still need
native qualification. No package build or closure audit is established by this
source unit. The existing tinycc development-shell mismatch and missing native
builders remain separate blockers; no pin, hash or dependency update is implied.

The hosted Rust workflow uses Rust 1.95.0 with rustfmt and Clippy, verifies fixture
prerequisites and sets the lexical GNU logname entry before compiling. It checks
all targets/features and lints them, then runs default tests with qualification
mains disabled and ignored native tests left ignored. Toolchain/crate downloads
are future hosted CI behavior. Local source checks do not establish an Ubuntu
runner result, pinned production-coreutils parity, real Nix builds, site migration
or operator acceptance. Ruby workflows and the self-hosted suite remain separate.

The self-hosted native VM/GC job runs automatically only for default-branch
pushes matching its existing path filters. Feature branches, including Ruby
changes, and tags require deliberate `workflow_dispatch` with separate runner
ownership and authorization. The guard skips the whole job before runner
allocation, including GC reset and cleanup; a skipped job is not a native
test pass. Older refs retain their existing scheduling until they adopt the
guard. Hosted Rust, RuboCop and RSpec workflows keep their existing triggers.
