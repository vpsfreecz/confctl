# Status without a target build

`confctl-rust status --generation none [machine-pattern]` compares resolved target
input revisions with deployed input metadata. It checks managed machines with a
target, including carried machines through their carrier. Review the preview
and confirm, or pass `--yes`. An empty selection fails.

This mode evaluates each machine's resolved input metadata without building a
target system. Omitted generation and saved-generation status remain unavailable
in the Rust CLI. The default `confctl` entry point remains Ruby.

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
roles alone do not make the host outdated.

Offline, outdated and unknown observations still allow exit status 0. Diagnostics
identify unavailable input metadata and failed native queries. Configuration,
process infrastructure, protocol decoding, cancellation and output failures fail
the command. Correct the configuration or connectivity issue and run the read
again; the command does not poll or repair state.

Status reads local generation records and reports exclusions without changing
records or current links. It creates no mutation lock or GC roots, reads no
current-system path, and performs no copy, activation, kernel refresh or profile
write. Normal inventory materialization, metadata evaluation and invocation
logging still occur.
