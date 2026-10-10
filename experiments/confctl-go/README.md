# Experimental confctl Go CLI

`confctl-go-prototype` is the separately packaged experimental Go CLI. The normal
`confctl` package remains the operational tool. This package executes help,
`init`, `add`, `rename`, `rediscover`, `ls`, `status --generation none` and
explicitly registered external extensions. Other commands and generation modes
fail before opening logs, evaluating Nix, running SSH or changing configuration.
Root help shows the reference builtin inventory. Detailed help describes the
execution limits of unavailable commands and status modes.

The Go module `github.com/vpsfreecz/confctl` and its `cmd`, `internal` and
`extension` packages live at the repository root. The normal Go package builds
only `confctl-go-prototype` and `hook-driver`, with no site executable, registry
or configuration dependency. This directory retains the opt-in package
definitions and this capability table.

Build the explicit package with `nix build --no-write-lock-file
.#confctl-go-prototype`. Unit checks run through its package check phase, or in
`nix develop --no-write-lock-file .#experimental` with `go test ./...` and
`go vet ./...` from the repository root. `go test -race ./...` is a separate check.
The compatibility driver and immutable Ruby observations live in
[tests/compat](../../tests/compat/README.md); the source catalogue is in
[docs/compatibility](../../docs/compatibility/README.md). Builds and comparison
results are snapshot evidence; this README makes no performance claim.

The configuration-owned site package supplies its executable and registry
declarations. `fixture-package.nix` requires explicit `pkgs`, `fixtureTools`,
`sitePackage` and `registryTemplate` inputs. Only this fixture composition
substitutes `@SITE@` with the supplied package and installs
`share/confctl-go-prototype/registry.json`. That template has empty
`bound_sources` and is not a valid operational registry. The compatibility
driver copies it outside the configuration tree, binds the actual
fixture `flake.nix` bytes and supplies both `CONFCTL_EXTENSION_REGISTRY` and
`CONFCTL_EXTENSION_ROOT`. Original Ruby runs use no candidate preparation.
The test-only `hook-driver` uses the same bound loader and executes
`rediscover.after-write` and `deploy.prepare`; deployment remains unavailable.
Native rediscovery invokes `rediscover.after-write` after replacing the inventory,
including rediscovery called by add/rename. Netboot output uses
`cluster/netbootable.nix`; kernel state retains `configs/node/kernels.json`.
The configuration-owned handlers are separate executable processes using the
[public experimental SDK](../../extension/README.md), including when the registry
points to the same site binary for both registrations. The Go CLI does not load
Ruby files from `scripts/`.

Ordinary module/package checks have no site dependency. The two executable
state/error-order cases are core-owned conformance tests with an explicit
external binary:

```sh
CONFCTL_TEST_SITE_EXECUTABLE=/absolute/path/to/vpsfree-confctl-ext \
  go test -tags siteconformance ./internal/core \
  -run '^TestKernel(ErrorValueRetainsPriorAndAbsentKeys|FailureDetailsUseCompletionOrder)$' -count=1
```

Requested conformance fails when the supplied executable is absent or missing;
it never compiles a bundled handler or skips that dependency. The tests use the
actual core supervisor and a disposable SSH endpoint. Pure and public-protocol
site tests belong to the configuration-owned module.

One typed command tree in `internal/cli` owns all builtin groups and 29 leaves,
options, defaults, aliases, argument usage and availability. It generates help
and drives parsing and the execution guard. Existing help text files are test
references; the executable does not load them. Root and `ls` help retain their
reference bytes. Detailed status help limits execution to `--generation none`;
other unavailable leaf and group help identifies the current capabilities.
Help validates the explicit registry, live root and finite declared source
bytes, and runs no tools or extensions.

Parsing stops at the first positional or `--`, preserving the remaining tokens
for the handler and log. A later `--help` is a literal argument. Displayed usage
does not add argument-count validation to existing cluster handlers. Explicit
empty options, repeated filter order, short clusters, declared negation and
string versus integer options remain distinct. Command help returns as soon as
its option is parsed; earlier errors still win. Integer options preserve Ruby's
signed radix and separator syntax and arbitrary precision. Nix count options
remain strings. Framework abbreviations and full terminal-width parity remain
unverified.

Schema1 registrations now declare general static groups, executable commands,
option aliases/types/defaults and required/optional/variadic arguments through
the same command tree. Both authority variables absent means builtin-only;
partial/empty pairs, root mismatches, stale or symlinked bound files, invalid
declarations and collisions fail before help, logs or external processes.
A hook-only registry adds no command. Help works when an absolute declared
executable is missing. Arity is checked before execution. Only the finite
`bound_sources` bytes are checked; unrelated edits are allowed and there is no
automatic rebinding or concurrent-edit lock. The public [SDK contract](../../extension/README.md)
describes default presence and exact JSON-number options. The existing
runtime-kernels payload/log adapter, services, supervisor and hook-driver
boundaries remain unchanged. No production registry is adopted.

Configuration commands retain the Ruby templates and filesystem order. Init
permits only `shell.nix`, `.confctl`, `.gems` and `.gitignore` in the initial
directory; `.git` is rejected. Add/rename require a flake before validating their
arguments. Rename moves files without rewriting embedded names or imports.
Rediscovery follows directory links, includes hidden/nested paths whose
`module.nix` and `config.nix` exist, and sorts relative paths. Replacement writes
`cluster.nix.new-<six hex digits>` before renaming it. Failed writes or hooks leave
completed files in place; there is no transaction or automatic undo.
Init directory creation and new files respect umask. Add and rename use
FileUtils's explicit `mkdir_p` mode: newly created directories become 0755,
existing directory modes are preserved, and new files still respect umask.

Rediscovery invalidates settings and inventory before its first hook and after
each subscriber, including failure. Hooks run in order, extension ID and handler
order through the existing supervisor. Their context carries the actual origin
command/options/arguments/raw argv, empty action, null generation and no selected
machine restriction. Inventory stays lazy when no hook requests it. Separate
Configuration cases live under `tests/compat/stage2-b/fixtures`; their original
Ruby captures and acceptance remain distinct from the unchanged 41 references.

The following table is checked against the command registry:

<!-- command-registry-capabilities:start -->
| Builtin command | Execution |
| --- | --- |
| `add` | Available |
| `build` | Unavailable |
| `changelog` | Unavailable |
| `collect-garbage` | Unavailable |
| `cssh` | Unavailable |
| `deploy` | Unavailable |
| `diff` | Unavailable |
| `gen-data vpsadmin all` | Unavailable |
| `gen-data vpsadmin containers` | Unavailable |
| `gen-data vpsadmin network` | Unavailable |
| `generation ls` | Unavailable |
| `generation rm` | Unavailable |
| `generation rotate` | Unavailable |
| `health-check` | Unavailable |
| `init` | Available |
| `inputs channel ls` | Unavailable |
| `inputs channel set` | Unavailable |
| `inputs channel update` | Unavailable |
| `inputs ls` | Unavailable |
| `inputs machine set` | Unavailable |
| `inputs machine update` | Unavailable |
| `inputs set` | Unavailable |
| `inputs update` | Unavailable |
| `ls` | Available |
| `rediscover` | Available |
| `rename` | Available |
| `ssh` | Unavailable |
| `status` | Only --generation none |
| `test-connection` | Unavailable |
<!-- command-registry-capabilities:end -->

The prototype preserves the existing Nix and OpenSSH executable paths, status
worker count, carrier routing for builtin status, and the carried object's own
target for generic extension execution. Kernel updates retain unselected keys,
save successful results after handled host exit failures, and delete selected
failed hosts. A successful uname value normalized to `error` stays visible but
retains a prior saved value or leaves an absent key absent. The main table follows
inventory order; handled failure details follow worker completion order.
The deployment hook excludes carried machines. This does not
promise compatibility with arbitrary old Ruby scripts, GLI customization, or
all legacy exception classes.

Explicit invocation/context cancellation has an accepted experimental policy:
send TERM to the owned child group, allow two seconds, send KILL if it is still
alive, and wait/reap. It is not a routine command deadline. The test driver has
its own declared deadline and descendant cleanup. Cancellation is an invocation
failure and cannot be treated as a handled host failure followed by a state
save. No automatic retry, worker cap, output cap, new state lock or command
clock deadline is applied to successful comparison cells.

Malformed nonobject kernel state, missing targets and spawn errors are explicit
prototype errors. Legacy host `TTY::Command::ExitError` handling is characterized
separately; those broader exception categories are not claimed interchangeable.
Corrupt JSON fails without replacing state. Its initial invalid-value-starter
diagnostic follows the pinned JSON gem's token-fragment, line and byte-column
format; the other syntax-error classes remain unverified and use Go diagnostics.
The built-in generation reader is
bounded to current flake-generation records; it is not an implementation of
native build, deployment, rotation or legacy software-pin migration.

The SDK remains experimental. No production configuration pin selects the Go
CLI. Recovery is selecting the unchanged normal package while retaining
existing site state; no schema migration or daemon rollout is involved.
