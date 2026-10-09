# confctl Go measurement prototype

`confctl-go-prototype` is an experimental installed executable for comparing the
pinned Ruby implementation with Go. The normal `confctl` package remains the
operational tool. This package executes help, `ls`, `status --generation none`,
and the two registered site extensions. Other commands and generation modes
fail before opening logs, evaluating Nix, running SSH or changing configuration.
The full builtin help inventory remains visible as characterization material.

Build the explicit package with `nix build --no-write-lock-file
.#confctl-go-prototype`. Unit checks run through its package check phase, or in
`nix develop --no-write-lock-file .#experimental` with `go test ./...` and
`go vet ./...` from this directory. `go test -race ./...` is a separate check.
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
SDK](extension/README.md), including when the registry points to the same site
binary for both registrations.

Command registration is bounded to the packaged `runtime-kernels update`
shape: its group/command descriptions, option sets and optional machine-pattern
argument are static in the core. The registry must match those declarations;
other shapes and unknown registry fields are rejected before effects. A
hook-only netboot registry does not advertise the runtime-kernels command.

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
