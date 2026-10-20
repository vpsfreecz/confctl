# Status

`confctl-rust status --generation none [machine-pattern]` compares resolved target
input revisions with deployed input metadata. It checks managed machines with a
target, including carried machines through their carrier. Review the preview
and confirm, or pass `--yes`. An empty selection fails.

The `none` mode evaluates each machine's resolved input metadata without building
a target system. `confctl-rust status --generation <current|offset|name>` compares
against a saved local generation instead. Omitting generation builds a new
target before comparing it. The default `confctl` entry point remains Ruby.

Default `confctl-rust status [machine-pattern]` explicitly previews BUILD followed
by status checking and asks once. After consent, one mutation guard covers the
existing grouped build and publication; default build options apply. It performs
no deploy runtime-kernel refresh, copy, activation, profile or health operation.
Every build must succeed before observations begin, in original selected-host
order with task IDs after the build range. The returned generation object
supplies the target toplevel, while each host's inputs are evaluated from the
current configuration, including when an old generation with different saved
input metadata was reused. Local generation counts include completed publication.

Only this default mode can publish generations, roots and current links or
create the mutation lock. Build/publication/output/cancellation failure stops
before observations and retains earlier local effects. Later query failures
also retain publications. Inspect reported saved names and explicitly rerun;
the command performs no restoration or remote rollback.

Saved status resolves a generation for each selected host after the initial
confirmation. The preview shows found and missing hosts, generation names,
kernels and input revisions. If no host has the generation, the command fails.
Otherwise it asks before ignoring missing hosts; `--yes` accepts both prompts.
Missing hosts receive no remote reads or status rows.

`current` follows the validated local current link, or the newest supported
record when the link is absent. `0` and `-0` select the newest record, positive
offsets count from the oldest starting at 1, and `-1` selects the record before
the newest. Leading zeros are accepted; `+1` is a literal name. Out-of-range and
huge numeric offsets are missing. Numeric selection refuses when records were
excluded; rejected explicit names and broken current links also fail without
repairing state.

Saved status uses the record's input metadata and toplevel. It performs no
per-host target input evaluation and never substitutes today's configuration
for missing saved metadata. It reads uptime, then the running system's strict
realpath on standalone machines or the logical profile's strict realpath on
carriers, followed by profile generations and deployed inputs.

The command queries uptime and profile generations before reading deployed input
metadata. Standalone machines use `/etc/confctl/inputs-info.json`. Carried machines
first use a truthy `inputs-info` value from their profile's `machine.json`, then
fall back to the profile's `etc/confctl/inputs-info.json` when that value is absent,
null or false, or the metadata file cannot be read or parsed. A selected malformed
value stays unknown. A successful fallback retains diagnostics from failed
attempts. The host observation succeeds when its other required reads succeeded.

The table includes online state, uptime, comparison status, local:remote generation
counts and the sorted union of input roles. Unknown remote counts differ from
known zero. Role cells include `same`, `changed` or `unknown`, so comparisons remain
readable without color. Equality requires full non-null revisions with matching
types; matching short revisions are insufficient. Status is `ok` only when the
host is online and every role in a nonempty target map matches. Extra deployed
roles alone do not make the host outdated. Saved status additionally requires a
known running toplevel with exactly the same native path bytes as the saved
toplevel. Lexically equivalent paths are insufficient; an unknown running path
cannot match. A successful observation task can still report an outdated system.

Offline, outdated and unknown observations still allow exit status 0. Diagnostics
identify unavailable input metadata and failed native queries. Configuration,
process infrastructure, protocol decoding, cancellation and output failures fail
the command. Correct the configuration or connectivity issue and run the read
again; the command does not poll or repair state.

Explicit saved/none status reads local generation records and reports exclusions without changing
records or current links. It creates no mutation lock or GC roots and performs
no copy, activation, kernel refresh or profile write. The `none` mode does not
read the current-system path. Normal inventory materialization, configured
metadata evaluation in `none` or default mode and invocation logging still occur. Saved
record formats and the Ruby entry point are unchanged.
