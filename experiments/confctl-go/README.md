# Experimental confctl Go CLI

`confctl-go-prototype` is the separately packaged experimental Go CLI. The normal
`confctl` package remains the operational tool. This package executes help, `ls`,
`status --generation none` and the two registered site extensions. Other commands
and generation modes fail before opening logs, evaluating Nix, running SSH or
changing configuration.
Root help shows the reference builtin inventory. Detailed help describes the
execution limits of unavailable commands and status modes.

The Go module `github.com/vpsfreecz/confctl` and its `cmd`, `internal`, `extension`
and bundled `site` packages live at the repository root. This directory retains
the opt-in package definitions and this capability table.

Build the explicit package with `nix build --no-write-lock-file
.#confctl-go-prototype`. Unit checks run through its package check phase, or in
`nix develop --no-write-lock-file .#experimental` with `go test ./...` and
`go vet ./...` from the repository root. `go test -race ./...` is a separate check.
The compatibility driver and immutable Ruby observations live in
[tests/compat](../../tests/compat/README.md); the source catalogue is in
[docs/compatibility](../../docs/compatibility/README.md). Builds and comparison
results are snapshot evidence; this README makes no performance claim.

The packaged static registry is
`share/confctl-go-prototype/registry.json`. Set `CONFCTL_EXTENSION_REGISTRY` to
that absolute file to enable `runtime-kernels update`. The test-only
`hook-driver` executes `rediscover.after-write` and `deploy.prepare`; the
prototype has no native rediscover/deploy implementation. Netboot output uses
`cluster/netbootable.nix`; kernel state retains `configs/node/kernels.json`.
Both handlers are separate executable processes using the [public experimental
SDK](../../extension/README.md), including when the registry points to the same site
binary for both registrations.

One typed command tree in `internal/cli` owns all builtin groups and 29 leaves,
options, defaults, aliases, argument usage and availability. It generates help
and drives parsing and the execution guard. Existing help text files are test
references; the executable does not load them. Root and `ls` help retain their
reference bytes. Detailed status help limits execution to `--generation none`;
other unavailable leaf and group help identifies the current capabilities.
Help reads only the explicit registry JSON and runs no tools or extensions.

Parsing stops at the first positional or `--`, preserving the remaining tokens
for the handler and log. A later `--help` is a literal argument. Displayed usage
does not add argument-count validation to existing cluster handlers. Explicit
empty options, repeated filter order, short clusters, declared negation and
string versus integer options remain distinct. Command help returns as soon as
its option is parsed; earlier errors still win. Integer options preserve Ruby's
signed radix and separator syntax and arbitrary precision. Nix count options
remain strings. Framework abbreviations and full terminal-width parity remain
unverified.

Extension registration still accepts only the packaged schema-1
`runtime-kernels update` shape. Its validated metadata joins the same command
tree for parsing and help. Other shapes and unknown registry fields are rejected
before effects. A hook-only netboot registry adds no command, and help works
even when the declared extension executable is absent. The extension SDK,
invocation fields, services and hook-driver boundaries remain unchanged.

The following table is checked against the command registry:

<!-- command-registry-capabilities:start -->
| Builtin command | Execution |
| --- | --- |
| `add` | Unavailable |
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
| `init` | Unavailable |
| `inputs channel ls` | Unavailable |
| `inputs channel set` | Unavailable |
| `inputs channel update` | Unavailable |
| `inputs ls` | Unavailable |
| `inputs machine set` | Unavailable |
| `inputs machine update` | Unavailable |
| `inputs set` | Unavailable |
| `inputs update` | Unavailable |
| `ls` | Available |
| `rediscover` | Unavailable |
| `rename` | Unavailable |
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

The SDK module is unpublished and experimental. No production configuration pin
selects it. Recovery is selecting the unchanged normal package while retaining
existing site state; no schema migration or daemon rollout is involved.
