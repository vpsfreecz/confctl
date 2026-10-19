# Deploy saved generations

The Rust development binary supports `confctl-rust deploy --copy-only
--generation <selector> [machine-pattern] [action]`. The default `confctl` remains
Ruby. This route copies existing store paths only, even when reboot, activation
or health-check options are supplied. The non-copy-only route also supports
`confctl-rust deploy --generation <selector> [machine-pattern] boot` without
`--reboot`, and the same command with action `dry-activate` or `test`. Other actions, the
default switch action, new builds and rebooting remain unavailable before
application setup.

Selection includes managed machines of every spin, including carried machines;
it does not apply the runnable filter. The command previews hosts, selector and
action, then asks for confirmation. `--yes` skips this initial prompt. An empty
selection or declined confirmation starts no refresh or copy. Bulk copying
requires a positive `--max-concurrent-copy`; `--one-by-one` and `--interactive`
use serial copying.

After consent, one mutation guard spans optional runtime-kernel refresh and the
copy operation. When `runtimeKernels.refreshBeforeDeploy` is enabled, confirmed
non-carried vpsAdminOS hosts are refreshed before saved-generation lookup. An
empty refresh subset is skipped. The refresh can change the configured kernel
JSON even when lookup or copying later fails; the command retains this change.
Confirmed identities stay fixed and affected Nix caches are invalidated.

Generation selection applies separately to each host's date-ordered local
records. `current` uses its resolved current generation; `0` selects the newest,
positive `n` selects entry `n - 1`, and negative `n` selects entry `length + n - 1`.
Only an optional minus sign followed by decimal digits denotes an offset. The
original spelling is retained, including leading zeros and huge values; `+1`
is a literal name. Out-of-range offsets are missing generations. Rejected old
records make numeric selection ambiguous; unresolved current links fail current
selection. Neither records nor current links are repaired or rewritten.

The resolved table includes kernel and input revisions, with rows for missing
hosts. Missing hosts require an additional confirmation unless `--yes` applies.
Interactive resolved-list, missing-host and per-host confirmations are mandatory
even with `--yes`; declining a per-host prompt skips that copy. If no generation
is found, the command fails.

Localhost targets succeed without a copy child. Other targets run
`nix-copy-closure --to root@<target> <paths...>`; carried machines use their
carrier's target and port. Paths include the saved toplevel and, when enabled
and present, its auto-rollback path. `--disable-auto-rollback` wins over enable;
otherwise explicit enable or the machine policy selects it (enabled by default).
For a nondefault port, `-p <port>` is appended to inherited `NIX_SSHOPTS`.
An explicit null standalone target remains selected and visible, but fails its
copy task with a host configuration error and no copy child. Ruby instead tries
`root@`; attempted-child counts differ on that invalid configuration. Interactive
skip still precedes this check. A carried host's own null target is valid when its
explicit carrier has a destination; missing or null carrier destinations fail
without a fallback.

`CONFCTL_SSH_CONFIG` applies to optional kernel probes, not this copy transport.

Bulk admission visits standalone hosts first, then interleaves carried machines
round-robin by carrier, within the concurrency bound. Ordinary copy failures are
collected after the admitted copies finish; serial failure stops later hosts.
Infrastructure, output failure and cancellation stop admission and join active
children. Cancellation exits 130 unless an earlier primary failure takes
precedence. Already delivered host results remain actual observations.

Each admitted copy has its own pending, entered and final task events. Stderr
and bounded copying-count/path progress use the existing checked output relay;
unknown totals remain indeterminate and raw captures/logs retain their bytes.
Recognized count/path activity also produces host-labelled copy lines through
the same relay, so redirected output retains per-host progress. Arbitrary raw
chunks produce no extra diagnostics, and progress never declares copy success.
Partial remote store copies and earlier kernel-state effects remain after errors.
Generation records, GC roots and current links stay unchanged. Copy-only makes
no profile change, activation, reboot or health-check request. Controlled command
fixtures do not establish real-site copying, activation or full live-terminal
qualification.

## Prepare a saved generation for boot

After the same consent, optional refresh and generation confirmations, boot
queries resolved hosts with at most five concurrent read-only observations.
A standalone host is skipped only when its known running toplevel and its known
current system profile both match the saved toplevel. A carried host uses the
known current carrier-profile path alone, even if a later profile-list read
fails. Native read failures produce diagnostics and leave those observations
unknown; unknown state never means already current. Configuration, decoding,
capture, output and cancellation failures stop the invocation. Copy-only does
not perform these status queries.

Bulk mode finishes all copies successfully before activating any host. It then
visits copied hosts sequentially in their original selection order. Interactive
bulk mode confirms and copies each host serially before starting this activation
phase; declining a copy also skips that host's activation. `--one-by-one` instead
copies and activates each host before moving to the next one. All known-current
hosts may be skipped successfully without copying or activation.

Standalone hosts execute `<saved-toplevel>/bin/switch-to-configuration boot`,
then, only after success, `nix-env -p /nix/var/nix/profiles/system --set
<saved-toplevel>`. Localhost executes as the invoking user. Remote commands use
root SSH, the configured port and nonempty `CONFCTL_SSH_CONFIG`, separately from
the copy transport's `NIX_SSHOPTS`. Carried boot only copies store paths; it does
not activate the carried system or update its profile.

`--dry-activate-first` runs `dry-activate` before standalone boot. With
`--interactive`, the normal-echo activation choice has no default: `y` continues,
`s` skips and `a` aborts; `r` repeats the dry run only when dry-first is enabled.
Invalid or empty answers repeat the choice. `--yes` does not bypass it. Carried
boot adds neither a dry run nor an activation prompt.

Each standalone activation task covers its required ordered commands and only
succeeds after profile setting. Native output and statuses retain their existing
capture and delivery ownership. Failure stops later activation; cancellation
joins entered children and is checked after acknowledged output and results.
Earlier refreshes, copies, bootloader changes and profile updates remain. Boot
success followed by profile failure is a partial result, without rollback or a
boot-readiness guarantee. Inspect target state before retrying the saved
generation or deliberately choosing an older one.

Saved records, local current links, GC roots and remote formats stay unchanged.
The mutation guard does not coordinate with Ruby or other deployment tools.
Boot without reboot runs no rollback helper, reboot, wait-online or health check,
regardless of their parsed options. Native success does not establish that a
later reboot will succeed. The default Ruby entrypoint and production cutover
are unchanged.

## Dry-activate a saved generation

Use `confctl-rust deploy --generation <selector> [machine-pattern] dry-activate`
to run the saved toplevel's `bin/switch-to-configuration dry-activate` script.
This uses the same consent, mutation guard, optional runtime-kernel refresh,
generation lookup, status-query order and copy scheduling as boot. Standalone
hosts skip only when the known running path matches the saved native path bytes;
the profile need not match, and a later native profile-query failure does not
erase that observation. Unknown running state never skips. Carried hosts keep
their known current carrier-profile predicate and only receive copies.

Standalone dry activation runs no profile setter or other activation action.
With `--dry-activate-first`, Continue intentionally runs two dry commands: the
preliminary command and the requested command. Retry repeats the preliminary
command before offering the choice again. The same mandatory interactive
choices, skip-after-copy behavior, bulk barrier and one-by-one ordering apply.
`--yes` does not bypass activation choices. Native failure or cancellation stops
later activation and retains earlier effects.

Dry activation is not a preview without effects: kernel-state refreshes, store
copies and effects of the saved script can remain. Inspect partial results
before an explicit rerun. Success means that the requested dry command completed;
it does not guarantee that a future switch or reboot will succeed. This route
runs no rollback helper, profile change, reboot or health check and leaves local
saved records, current links and GC roots unchanged.

## Test a saved generation

`confctl-rust deploy --generation <selector> [machine-pattern] test` uses the
configured rollback policy and health checks by default. Known running paths
skip using the same exact native-byte predicate as dry activation; unknown
state does not skip. Carried hosts receive copies only. Test sets no profile
and performs no reboot or new build.

Bulk mode finishes all copies, then activates standalone hosts sequentially,
then checks the successfully activated, unskipped hosts in selection order.
One-by-one mode finishes each host's activation and health policy before the
next copy. Declined copies, known-current hosts and skipped activations receive
no health checks. Activation failure stops before the later bulk health phase.

Enabled rollback with a saved helper requires successful confirmation and its
joined successful result before health starts. Disabled rollback, a configured
off policy or a missing saved helper uses direct native test; missing protection
is reported. Health neither proves the helper's hidden inner switch result nor
triggers rollback on failure. The helper protocol is described below.

Health compilation includes every configured kind on completed runnable hosts;
carried and null-target machines are excluded. Active malformed metadata fails
with earlier effects retained. Empty checks are a no-op. `--no-health-checks`
bypasses compilation, prompts and execution. Deploy health uses five workers;
`--max-jobs` remains a Nix build option, deliberately replacing Ruby's use of
that build-option value as a health worker count. The standalone health-check
command keeps its own concurrency option.

Interactive health consent is mandatory even with `--yes`. Decline continues
with an explicit unverified result. Failed batches offer `c` Continue, `r` Retry
all, `f` Retry failed or `a` Abort. Empty input aborts, making the advertised Abort
default effective; invalid input repeats, and EOF fails. Retries borrow the
original compiled checks and arguments, with fresh task IDs, and perform health
only. Failed tasks and raw logs remain retained after eventual success or
Continue. `--keep-going` accepts ordinary comparison failures only in
noninteractive mode; it cannot bypass interactive choices or configuration,
process, capture, delivery or cancellation errors.

Summaries distinguish passed, declined and continued-after-failure rounds.
Refreshes, copies, scripts and check programs can leave effects after any later
failure or cancellation. The existing guard spans the prompts and checks, while
local saved records, current links, roots and remote formats remain unchanged.
No compensating confirmation, rollback or profile command is sent on health
failure. Inspect results and target state before an explicit rerun. Controlled
fake-tool tests do not qualify real hosts or full live-terminal behavior.

## Rollback helper protocol

Saved test uses the existing rollback-helper protocol. Switch remains unavailable
through the Rust CLI. Copy, boot and dry-activate do not start this helper.

Copying and activation share the same rollback policy: disable wins, otherwise
explicit enable or the configured policy selects protection when a saved helper
exists. An absent saved helper falls back to direct activation with a notice.
The enabled caller requires the configured signed integer `autoRollback.timeout`;
missing or malformed values fail rather than receiving a default. Each invocation
uses a fresh `/run/confctl-confirm-<six random hex digits>` path and passes the
saved helper native arguments `-t <timeout> <toplevel> <action> <check-file>`.
Local commands run as the invoking user; remote commands use the existing root
SSH, port and nonempty `CONFCTL_SSH_CONFIG` behavior. Carried hosts never activate.

One owned helper runs alongside serial marker reads and confirmation commands.
Reads compare complete captured bytes after removing ASCII whitespace and NUL
only at the edges. The caller polls immediately and then at interruptible
one-second intervals. `switched` permits confirmation; `switching` keeps waiting.
For unavailable or other markers, elapsed timeout plus ten seconds permits the
legacy fallback, with a diagnostic that `switched` was not observed. This
monotonic threshold is not a deadline that kills the helper or an SSH request.
Negative timeouts remain signed.

The caller attempts confirmation at most ten times, retrying only complete
native failures and retaining failed-attempt diagnostics. Success requires a
successful confirmation command and a complete helper exit of zero. This
deliberately differs from Ruby, which can accept helper zero after all ten
confirmations fail. Early helper completion without confirmation also fails.
On exhaustion, the caller stops writing and awaits the helper's natural result.
An entered control request is settled before return, and helper completion stops
new requests.

Neither a marker nor helper zero proves the hidden switch or rollback exit
status: the existing helper ignores those child statuses. The result describes
the rollback-helper protocol only. Infrastructure, capture, delivery and
cancellation failures stop new control work and join owned local requests.
The remote watchdog can outlive its SSH transport; a local join does not prove
remote rollback or closure. Failure sends no compensating confirmation, deletes
no check file, retries no activation and sets no profile. Inspect the target and
partial effects before an explicit later deployment. Saved records, current
links, roots, helper bytes and the remote protocol remain unchanged. Confined
Supervisor tests do not qualify the protocol against real hosts or establish
full terminal behavior.
