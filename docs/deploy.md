# Copy saved generations

The Rust development binary supports `confctl-rust deploy --copy-only
--generation <selector> [machine-pattern] [action]`. The default `confctl` remains
Ruby. Requests that build a new generation or activate it remain unavailable
before application setup. This route copies existing store paths only, even when
reboot, activation or health-check options are supplied.

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
Generation records, GC roots and current links stay unchanged. This route makes
no profile change, activation, reboot or health-check request. Controlled command
fixtures do not establish real-site copying or full live-terminal qualification.
