# Core compatibility oracle

## Candidate comparison

The [behavioral contract](../../../docs/compatibility.md#candidate-acceptance-by-consumer)
preserves actions/effects, status and useful information without Ruby human-frame
byte parity. Core candidates return MATCH, DIFFERENT or UNQUALIFIED.
Ordered OS child calls, paths/modes/link targets, typed outcomes and persisted
JSON values remain checked. Closed table/diagnostic profiles allow documented
presentation differences; unrecognized prose or changed file-reader semantics
stay UNQUALIFIED. Known operational field/header/list output keeps complete record meaning.
Help/usage stays separate; unknown reflow is UNQUALIFIED. Parser refusal never
falls back to token equality, and only LF/CRLF records are interpreted.
JSON conversion/depth limits are UNQUALIFIED with an explicit reader-limit reason.
LOCK-JSON-01 provenance remains unchanged; original observations are read-only.

Raw captures never change. Mixed SSH display/child bytes and opaque boot/wire
payloads need their separate raw consumer evidence; changed normalized text is
insufficient. Timing/live TUI, all helper/site adapters and native/monitoring
limits retain their own gates. The new focused comparator cases exercise
presentation differences and rejection of wrong targets/effects/statuses,
typed values, missing information and broken field records; they do not qualify
a product or run the fixed compiler batch.


`baseline.json` records the historical revision, tree and archive hash. The
original application/archive and Ruby execution/recording modes are retired.
Candidate replay does not need that source, Ruby or Bundler. Frozen JSON and its
provenance remain unchanged. The source can be recovered from the recorded Git
revision when historical investigation is explicitly needed.

Run an explicit candidate in the declared development shell:

```sh
python3 tests/compat/core/run.py --candidate /absolute/path/to/confctl --case ls-default
python3 tests/compat/core/qualify_settings.py --candidate /absolute/path/to/confctl --evidence /owned/evidence/settings
python3 tests/compat/core/qualify_channels.py --candidate /absolute/path/to/confctl --evidence /owned/evidence/channels
python3 tests/compat/core/runner_test.py
```

Select non-API cases explicitly. API routes use the Rust command adapter's
confined transport and cannot be qualified by injecting the old Ruby preload.
The main comparator retains raw separate stdout/stderr, native exit/signal,
ordered child calls and final files with `--evidence`. It never drops an
arbitrary diagnostic, status or column. Candidate execution never rewrites
expected data or the declarative `coverage.json` inventory.

Settings and channel candidate adapters retain their lifetime, lock, child-argv,
error and retained-log assertions. The channel adapter still checks the frozen
LOCK-JSON-01 baseline and override hashes before applying its named decision.
Old follows/login recorder assertions are covered by the existing Rust input
mutation, lock graph, generation/login and native-byte path tests. Their frozen
JSON remains the historical reference. See the
[retirement assertion map](../../../docs/compatibility-site.md).

The pure Python regression suite tests comparison failures as well as successful
replay through a real confined candidate without any source archive. This checks
the runner itself; it does not certify a newly built product or native helpers.

## Evidence and preserved decisions

Each candidate invocation gets a temporary cwd/home and fake child directory
first on PATH. Known Nix/Git/SSH calls use `child.py`; unexpected requests are
recorded and fail the case. SSH accepts only fixture targets. The fake Nix has no
evaluator or daemon connection. The runner kills only its owned process group
on the 15-second fake-case deadline and joins it. That deadline is a harness
failure, not an application timeout contract.

Normalization replaces only owned case/normalization directories, logger filename
timestamps and named temporary-file nonces. Fixed generation names and saved
timestamps remain compared. Unknown timing appendices stay UNQUALIFIED; timing
meaning and live presentation need their owning tests. No stderr is discarded.

GC-01 retains two original worker crashes as historical defects, not expected
candidate failures. Of the 372 cases, 370 have ordinary snapshots and two are GC
defect observations. Candidate success requires the same child boundary and exit
0; genuine failed-child GC requires exit 1. Rust mixed-host, renderer-fault and
join tests own infrastructure precedence and cleanup beyond that predicate.

The eight frozen follows observations retain the first-segment array-reference
behavior, saved original metadata, unchanged nested leaf and missing-first-node
refusal. `follows-manifest.json` binds those observations to their source revision
and checksums. The ten native-byte login/join observations cover empty/nil login,
leading/embedded/trailing/repeated slashes, dot segments and non-UTF8 bytes.
They establish path joining, not libc failure or USER fallback.

Ten settings observations distinguish invocation defaults from later per-client
settings, child construction order and original failures. Twenty channel
observations retain selection order/duplicates, empty/array mappings, typed cells,
settings lifetimes, child/lock errors, surplus arguments and the unchanged
cooperating writer. Retained log names/modes are exact; raw log bodies are stored
separately. Only `channels-corrupt-lock` uses the frozen full
`expected-channels-candidate/LOCK-JSON-01.json` frame, with baseline and override
hashes checked and stderr as its sole changed field. That decision grants no
wider parser, coordinate or I/O diagnostic equivalence.
