# Local generation listing

The Rust CLI supports `confctl-rust generation ls [machine-pattern]
[generation-selector]` for local build generations. Local listing is the default;
`--local` selects the same records. Requests containing `--remote`, including
`--local --remote`, fail before configuration or log setup because the Rust CLI
does not yet query remote generation profiles.

Machine names, keys, patterns, attributes and tags use the normal inventory
selection rules. Unmanaged and carried machines are included. Listing reads
existing generation records and current links without acquiring the mutation
lock, resolving the login name or creating GC roots. The application still
writes its normal invocation log. The default `confctl` entry point remains Ruby.

## Generation selectors

Hosts retain inventory selection order, with each host's records in date order.
The selector applies to that combined list before rows are sorted by generation
name and host for display.

| Selector | Meaning |
| --- | --- |
| omitted | All supported local records |
| `current` | Each host's current local generation |
| `old` | Records other than each host's current generation |
| `0` | Last entry in the combined list |
| positive `n` | Entry at index `n - 1` |
| negative `n` | Entry at index `length + n - 1` (`-1` is the second last) |
| `Nd` | Records strictly older than `N` days, using one invocation time |
| other pattern | Existing generation-name pattern matching |

Numeric selectors accept an optional minus sign and decimal digits. If the
offset is out of range, the original text becomes a name pattern, including
leading zeros. `+1` is a name pattern. An empty result succeeds.

Listing reports excluded records with their paths and reasons. Numeric selection
fails if any selected host has excluded records, since excluding records can
change an offset's meaning. Selecting an excluded record by name also fails.
`current` and `old` fail when a current link cannot be resolved. When the link is
absent, the existing loader treats the newest supported record as current.
Listing never repairs records or current links. See the
[upgrade guide](swpins-to-flakes.md) for unsupported software-pin generations.

## Output

Columns are `HOST`, `NAME`, `ID`, `PRESENCE`, `CURRENT`, `KERNEL`, followed by
input roles in their first-seen order among selected generations. Local IDs are
empty; presence is `build`, and current is `build` or empty. Kernel versions come
from the saved toplevel's `kernel` or `bzImage` symlink. Missing links leave an
empty kernel cell.

Input revisions accept saved string and object forms. Objects use `shortRev` or
its `short_rev` alias, then the first eight characters of `rev`. Listing retains
unknown saved metadata and never rewrites the records.
