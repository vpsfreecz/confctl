# Generation removal and garbage collection

`confctl-rust generation rm [machine-pattern [generation-pattern]]` removes the
selected generations. Selection includes unmanaged and carried machines and uses
the same local/remote view as [generation listing](generation-listing.md).
The default location is local. `--remote` selects remote profiles only; use both
`--local --remote` to include both. Current generations can be explicitly removed.

The confirmation table sorts a display view; removal follows the original unified
order, remote sources first in selected-host/profile-ID order, then local records
by stored date. Entries merge only with the existing host/toplevel/name/input
rules. Each unified entry removes local records first and remote profiles second.
Global numeric offsets, current/old, strict `Nd` age and patterns match listing.
Excluded records stay untouched. Ambiguous numeric selection, explicit rejected
names and unresolved current/old selection fail before removal.

`confctl-rust generation rotate [machine-pattern]` applies buildGenerations
retention to local records and hostGenerations retention to remote records.
Absent, null and false host values fall back to global settings; zero is a value.
Rotation uses one planning time, supported-record counts, signed min/max/maxAge,
and a strict age boundary. It never selects current. Current can keep the count
above max. Remote removals run before local removals; equal toplevels do not merge
for rotation. A broken local current link refuses rotation even for an empty plan.

Rm and rotate acquire the canonical configuration's mutation lock before reading
generations, and hold it during confirmation, removal and all optional GC joins.
The lock can be present while the operator decides; declining starts no cleanup.
It coordinates cooperating Rust writers, not Ruby, other machines or external
editors. No second generation query or current-link repair occurs after consent.
An empty plan reports no generations and runs no GC. `--yes` accepts confirmation.

Local removal validates the guard, root namespace, supported record and real
parent directories. It unlinks only that generation's existing GC roots, then
its toplevel, optional rollback and input links, generation.json, and empty
generation directory. The original per-user login/shortHash namespace is retained.
Store targets, current links, host/root directories, rejected records and unknown
files are retained. An unknown extra file makes the final nonrecursive rmdir fail.
There is no recursive removal or migration of old records.

Remote removal uses the recorded numeric profile ID with `nix-env -p <profile>
--delete-generations <id>`. Carried hosts use `carrier-env` on their carrier with
the logical profile. Its callback can fail after a profile has changed; an outer
failure does not prove that no deletion happened.

After successful removal, `--remote` with GC enabled collects only changed logical
hosts, resolves carriers and deduplicates targets. `--no-gc` disables this step;
local-only removal does not run GC. Rm ignores retention GC policy. Rotation keeps
Ruby's unusual condition on the resolved target:
`target.buildGenerations.collectGarbage || global.hostGenerations.collectGarbage`.
Ruby truthiness applies: false does not override global true, and the logical
host's hostGenerations override is not consulted. An empty target set is a no-op.

`confctl-rust collect-garbage [machine-pattern]` selects runnable machines,
including unmanaged machines. It confirms first, then acquires the same guard.
Each target runs exactly `nix-collect-garbage`, without `-d`. The default concurrency
is 5. Enabled GC rejects nonpositive or unrepresentable concurrency before deletion;
local-only and `--no-gc` routes ignore an unused GC limit.

Both native output streams appear live with host labels. Only exact bounded,
decodable `deleting '/nix/store/…'` lines count as deletion activity. Ordinary,
invalid-byte and overlong output stays visible but uncounted; total is unknown.
Native summary text is retained separately and never proves success or changes
the activity count. Native exit status decides the result. Ordinary host failures
allow other GC targets to run and are aggregated. Infrastructure, output or
cancellation errors stop admission and join every entered child and relay under
the guard. Raw captures and logs retain the undecorated native bytes.

The Rust route deliberately fixes Ruby's ordinary-output Progress#line exception
and its counting of non-path lines. Historical Ruby fixtures remain unchanged.
The default executable remains Ruby and the saved record format is unchanged.

Removal and GC can leave irreversible partial effects. An error stops later
removals and optional GC; earlier root, record, profile, callback or store changes
can remain. Inspect remaining generations and current links before choosing a
new command. A blind retry can fail on an already partial record. Rolling the CLI
back does not restore deleted objects; missing store objects require rebuilding
or restoring them.
