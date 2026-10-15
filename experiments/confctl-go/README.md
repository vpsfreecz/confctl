# Experimental confctl Go CLI

`confctl-go-prototype` is the separately packaged experimental Go CLI. The normal
`confctl` package remains the operational tool. This package executes help,
`init`, `add`, `rename`, `rediscover`, `ls`, `inputs ls`, `inputs channel ls`,
`inputs update`, `inputs channel update`, `inputs machine update`,
`inputs set`, `inputs channel set`, `inputs machine set`,
`status --generation none`, `generation ls`, `test-connection`, `ssh`, `cssh`
and explicitly registered external extensions. Other
commands and generation modes fail before opening logs, evaluating Nix, running
SSH or changing configuration.
Root help shows the reference builtin inventory. Detailed help describes the
execution limits of unavailable commands and status modes.

`test-connection` checks selected machines sequentially. It captures uptime
command output and reports successful and failed hosts. Completed host failures
keep the overall exit status 0; start, wait or cancellation errors stop the
invocation and retain its log.

`ssh` selects managed runnable machines and retains literal OpenSSH arguments,
including `CONFCTL_SSH_CONFIG` and per-machine ports. Commands run serially or
with one worker per selected machine. Parallel results follow completion order;
aggregate groups retain first-seen order and sort names within each group.
Command output merges stdout and stderr at the child descriptors. A handled
nonzero host status still prints the original separate error result, which can
be empty after merging, and keeps the overall exit status 0.

Absent command input leaves its pipe open. Explicit empty `--input-string`
supplies EOF; input options conflict even when empty. Each host reopens
`--input-file`. The pinned command dependency treats a nonexistent pathname as
literal input in a temporary file, and the Go adapter retains that behavior.
Terminal parallel output displays recent command-log lines and advances progress
as each host finishes. `CONFCTL_TTY=0` or a pipe on stdout/stderr selects the
noninteractive header instead.

Exactly one positional argument opens an interactive shell on one machine.
Interactive SSH inherits stdin/stdout/stderr and the caller's process group,
without forcing OpenSSH PTY allocation. Localhost resolves UID0's shell through
`getent passwd 0` and executes it as the current user. The runtime environment
must supply `getent`; there is no `$SHELL` or hardcoded-shell fallback.
Interactive child nonzero status is ignored. Explicit cancellation targets only
the owned child handle, with TERM, two seconds of grace, KILL and one Wait; it
never signals the caller's process group. Terminal attributes are restored while
the caller owns foreground. Cancellation does not prove descendant or remote
termination.

`cssh` retains the literal `nix-shell -p perlPackages.AppClusterSSH --command`
launch and its `cssh -l root <targets>` command string, without per-host ports or
SSH configuration options. The pinned command dependency converts `shellHook`
with a nil value to a present empty export. The adapter keeps those bytes and
ignores completed child nonzero status. Local fake-child and PTY tests exercise
these adapters; original-family compatibility acceptance and packaged SSH/GUI
integration remain separate verification steps.

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

`inputs ls` reads the sorted names under `nodes.root.inputs` in `flake.lock`
without evaluating Nix. An array reference uses only its first element when that
element is a string. The first positional token filters input names; later
positional tokens are ignored. `inputs channel ls` accepts at most one
selector and evaluates `confctl.channels` with the existing settings and impure
policy. Patterns sort matching channels; comma/brace lists preserve their literal
order and repeated names. Role rows preserve the returned mapping order.
Both commands require `flake.nix` before reading the lock or validating handler
arguments. Failures retain logs; successful reads leave the lock bytes unchanged.
The lock reader translates the observed initial invalid-value JSON diagnostic;
other malformed JSON classes keep Go diagnostics and are not claimed equivalent.
Separate original references live in `tests/compat/stage3-i1`.

Input updates preserve selection order and deduplicate channel input requests in
first-seen order. Root `--all` uses sorted root inputs and ignores positional
names; explicit names may create a missing lock. Update invokes literal
`nix flake update` without evaluation flags, with one experimental-feature retry
only for a matching failed command. The specific cached GitHub metadata warning
is an error even after exit0. Failures retain written lock bytes. Diffs compare
revisions only; metadata-only changes report no changes and skip commit.

Input setters accept a literal revision and preserve each selected input's
original source metadata after every successful lock operation. Channel set
refuses inputs shared with other channel/role pairs unless `--allow-shared` is
set; its warning is printed before mutation. Channel and machine summaries
reload the final lock after any optional commit. A later failure retains earlier
lock replacements and logs, without automatic rollback or repair. Inspect those
retained changes before retrying or returning to the operational Ruby tool.
Revision-only diffs skip optional commit when no revisions changed.

Optional commits use the existing message grouping and actual-URL mirror cache
under the private XDG/HOME cache. Ordinary mirror failures become changelog text;
cancellation propagates and prevents commit. Git commit inherits stdin/stdout/
stderr directly and adds `-e` only when editor is enabled and stdout is a TTY.
It performs no `git add` or rollback, and Git owns index, hooks and editor behavior.
The Linux commit adapter observes the exact Git child's stop/continue events.
A private packaged Bash helper relays terminal stops to the unchanged caller
job and reports its actual inherited signal state. A matching eligible ACK
permits owned Git continuation, with terminal handoff only when the caller owns
foreground. Restoration preserves another live foreground group. Helper,
observer and terminal failures retain their diagnostics; ordinary Git failures
remain `git commit failed`. Stop/resume, initial background jobs and real editor
parity still require the original/candidate PTY acceptance checks.
Real optional-commit job-control, editor and cancellation acceptance remains
blocked. The experimental setters expose all declared commit options; source
unit tests and fake processes do not establish installed setter parity or
production readiness.
Separate original-only references live in `tests/compat/stage3-i2`. Finite fake
Git tests establish process/argv boundaries, not real Git or terminal acceptance.

The following table is checked against the command registry:

<!-- command-registry-capabilities:start -->
| Builtin command | Execution |
| --- | --- |
| `add` | Available |
| `build` | Unavailable |
| `changelog` | Unavailable |
| `collect-garbage` | Unavailable |
| `cssh` | Available |
| `deploy` | Unavailable |
| `diff` | Unavailable |
| `gen-data vpsadmin all` | Unavailable |
| `gen-data vpsadmin containers` | Unavailable |
| `gen-data vpsadmin network` | Unavailable |
| `generation ls` | Available |
| `generation rm` | Unavailable |
| `generation rotate` | Unavailable |
| `health-check` | Unavailable |
| `init` | Available |
| `inputs channel ls` | Available |
| `inputs channel set` | Available |
| `inputs channel update` | Available |
| `inputs ls` | Available |
| `inputs machine set` | Available |
| `inputs machine update` | Available |
| `inputs set` | Available |
| `inputs update` | Available |
| `ls` | Available |
| `rediscover` | Available |
| `rename` | Available |
| `ssh` | Available |
| `status` | Only --generation none |
| `test-connection` | Available |
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
alive, and wait/reap. Inherited interactive SSH uses the child-handle exception
described above because it shares the caller's group. It is not a routine
command deadline. The test driver has
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

`generation ls` lists local flake generations by default. `--remote` queries
remote profiles, and `--local --remote` combines both sources. Machine filters
include unmanaged and carried entries; a carried profile uses its carrier's
connection and its own profile path. Listing queries uptime and generations
without reading the running toplevel or deployed input metadata.

Local readers retain ordered input metadata and exact JSON-number tokens. They
warn about rejected records without changing those records or their roots.
Numeric selection refuses excluded-record ambiguity; `current` and `old` refuse
an unresolved local current link. Remote-only selection leaves local records
unread. The local and remote counts in `status --generation none` use these same
readers, with a successful empty remote query distinct from a failed query.

Combined selection uses remote inventory and per-host ID order, followed by
local inventory and per-host date order. Display rows sort by name and host;
input columns follow their first occurrence before that sort. Numeric selectors
refer to the combined insertion order, with zero selecting the last entry and
minus one the penultimate. An out-of-range selector falls through to name
matching. For integers beyond a machine long, this bounded behavior differs
from the original Ruby array's `RangeError`. Age selection uses a strict cutoff
and retains timestamp fractions. Reads never repair current links or create
profiles or roots.

Default and selected-generation status, native build, deployment, generation
removal, rotation and legacy software-pin migration remain unavailable. Native
tests use disposable records and fake child processes. The finite original
adapters compare reader and table behavior; they do not establish packaged or
fleet parity.

The SDK remains experimental. No production configuration pin selects the Go
CLI. Recovery is selecting the unchanged normal package while retaining
existing site state; no schema migration or daemon rollout is involved.
