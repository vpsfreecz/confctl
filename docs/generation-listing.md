# Generation listing

The Rust CLI supports `confctl-rust generation ls [machine-pattern]
[generation-selector]`. Local build generations are the default; `--local`
selects the same records. `--remote` reads machine profiles, and `--local --remote`
combines remote and local generations.

Machine names, keys, patterns, attributes and tags use the normal inventory
selection rules. Unmanaged and carried machines are included. Listing reads
existing generation records and current links without acquiring the mutation
lock, resolving the login name or creating GC roots. The application still
writes its normal invocation log. Remote-only listing does not read local
generation storage. The public `confctl` entry point and `confctl-rust` alias both select Rust.

Remote listing queries uptime, then reads profile generations with a script sent
to `bash --norc` on stdin. Standalone machines use the system profile; carried
machines use their `confctl-<alias-or-clusterName>` profile on the exact carrier
from the inventory, replacing slashes with colons. The carrier's target, port
and optional SSH configuration apply. These queries do not add the connection
test's timeout options or install a helper on the machine.

An unavailable host has a failed task and a diagnostic identifying the failed
query and native status. Its profile list stays unknown, rather than becoming
a successfully read empty list. Ordinary host failures still let listing finish
with exit status 0, including when every selected host is unavailable. Spawn,
capture, decoding, cancellation and output failures fail the command. A missing
or dangling profile is an unavailable observation. A successful empty list
requires a resolved profile and a valid current-path response.

## Generation selectors

Remote records enter the combined list first, in host selection order and
profile ID order. Local records follow in host selection order and date order.
Records on the same host with the same toplevel merge; local records with
different names or inputs remain separate. A merged record retains the remote
ID and kernel, and uses the local name, date and input metadata. A remote record
without a local match uses a name derived from its timestamp in local time.
The selector applies to the combined insertion order before rows are sorted by
generation name and host for display.

| Selector | Meaning |
| --- | --- |
| omitted | All selected generations |
| `current` | Records current in either selected location |
| `old` | Records current in neither selected location |
| `0` | Last entry in the combined list |
| positive `n` | Entry at index `n - 1` |
| negative `n` | Entry at index `length + n - 1` (`-1` is the second last) |
| `Nd` | Records strictly older than `N` days, using one invocation time |
| other pattern | Existing generation-name pattern matching |

Numeric selectors accept an optional minus sign and decimal digits. If the
offset is out of range, the original text becomes a name pattern, including
leading zeros. `+1` is a name pattern. An empty result succeeds.

When local records are requested, listing reports exclusions with paths and
reasons. Numeric selection fails if any selected host has excluded records, since excluding records can
change an offset's meaning. Selecting an excluded record by name also fails.
`current` and `old` fail when a current link cannot be resolved. When the link is
absent, the existing loader treats the newest supported record as current.
Listing never repairs records or current links. See the
[upgrade guide](swpins-to-flakes.md) for unsupported software-pin generations.

## Output

Columns are `HOST`, `NAME`, `ID`, `PRESENCE`, `CURRENT`, `KERNEL`, followed by
input roles in their first-seen order among selected generations. Local-only IDs
are empty. Presence is `build`, `host` or `build+host`; current uses the same
labels, or an empty cell when neither location marks it current. Kernel versions
come from the toplevel's `kernel` or `bzImage` symlink. Missing links leave an
empty kernel cell. Remote profiles without local build records have no saved
input revisions to show.

Input revisions accept saved string and object forms. Objects use `shortRev` or
its `short_rev` alias, then the first eight characters of `rev`. Listing retains
unknown saved metadata and never rewrites the records.

For confirmed removal, retention and optional garbage collection, see
[generation cleanup](generation-cleanup.md).
