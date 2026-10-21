# Repository Guidelines

## Project structure

- `src/` owns the Rust CLI and four remote helper backends; concrete command
  adapters are under `src/operations/` and helpers under `src/remote/`.
- `bin/confctl` is a development launcher for the existing Cargo CLI target.
  Installed Nix wrappers execute compiled binaries directly.
- `nix/` owns packages, flake interfaces and NixOS/vpsAdminOS modules;
  `example/` contains a configuration example.
- `man/man8/*.md` contains manual sources; `examples/generate-reference.rs`
  maintains the CLI reference and renders explicitly supplied option exports.
- `tests/` contains Rust command tests, native VM suites and compatibility
  fixtures. Preserve `tests/compat/**` immutable Ruby sources and captures.
  Ruby native-runner extensions and VM test scripts are test infrastructure,
  separate from the retired Ruby application.

## Development and hooks

Enter `nix develop` for Rust tools and Nix formatting. Install the declared
repository hook in each clone before committing:

```sh
git config --local core.hooksPath .githooks
```

`.githooks/pre-commit` checks Rust formatting and tracked Nix files without
staging changes. This replaces Overcommit; do not bypass a failed hook.
`bin/confctl` invokes Cargo with the source manifest and preserves the caller's
cwd, arguments and exit status. Run it from the configuration directory you
intend to use, after entering the development environment.

Use Rust formatting and focused tests for changed behavior. Keep tests in their
owning module or existing command fixture; do not introduce another runner.
Run ordinary checks on the coherent source before final review:

```sh
cargo fmt --all -- --check
cargo check --workspace --all-targets --all-features --locked
cargo clippy --workspace --all-targets --all-features --locked -- -D warnings
```

After final review, run the default `cargo test --workspace --locked` suite and
assigned longer checks. All-feature checks compile qualification mains; do not
execute them or ignored native tests without a separate assignment. Native VM,
real-Nix, host and site operations require their own scope and prerequisites.
`CONFCTL_LOGNAME` must name the absolute lexical GNU logname entry before
compiling; preserve its multicall basename rather than resolving the symlink.

## Packaging and documentation

Read [the packaging guide](docs/rust-packaging.md) before changing packages,
helper producers or workflows. The public package and helper producers select
Rust. Ruby in configuration tools mode belongs to those repositories' tools and
must remain separate from confctl. Ruby extension APIs are removed; use the
existing typed settings and commands described in the project guides.

Markdown manuals are rendered with build-only Pandoc. Maintain the CLI reference
with `cargo run --locked --example generate-reference -- cli`, writing a complete
temporary candidate before replacing the checked-in page. Check drift with
`cargo run --locked --example generate-reference -- cli --check man/man8/confctl-reference.8.md`
and test the renderer with `cargo test --locked --example generate-reference`.
Follow the packaging guide for explicit option JSON input and safe regeneration.
Synthetic fixture JSON must not refresh the authoritative options manual.
Cargo fixtures cannot certify evaluated wrappers, installed manuals, package
closures, execution on both target architectures or site migration.

## Commits and releases

Read [`skills/confctl-release/SKILL.md`](skills/confctl-release/SKILL.md) before
preparing a version release. It covers Cargo/Nix metadata, reference maintenance,
package checks and separately authorized publication.

Commit subjects are short and imperative. Without a prefix, capitalize the
first word. With a `topic/component:` prefix, keep the first word after `:`
lowercase. Avoid trailing periods and bundle related changes together. Explain
the problem and resulting behavior. Write messages to a temporary file and pass
it with `git commit -F` or `git commit --amend -F`; wrap lines at 80 columns.
Keep validation transcripts in review/session records, outside the commit body.

For a `vpsadminos` flake-input update, read the current lock revision, run the
normal `nix flake update vpsadminos`, verify that only `flake.lock` changed, and
use `flake: vpsadminos <old9> -> <new9>` as the subject. Do not update inputs or
run native effects merely to satisfy an unrelated source change.
