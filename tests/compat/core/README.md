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
Baseline replay and LOCK-JSON-01 provenance remain unchanged.

Raw captures never change. Mixed SSH display/child bytes and opaque boot/wire
payloads need their separate raw consumer evidence; changed normalized text is
insufficient. Timing/live TUI, all helper/site adapters and native/monitoring
limits retain their own gates. The new focused comparator cases exercise
presentation differences and rejection of wrong targets/effects/statuses,
typed values, missing information and broken field records; they do not qualify
a product or run the fixed compiler batch.


`baseline.tar.gz` preserves the complete Ruby source at the revision in
`baseline.json`. The runner verifies its SHA256 before extracting it. It does
not resolve a moving Git branch, use the candidate library, or require the
retired source files to remain in the repository.

Run inside the repository's declared Nix development shell. The preserved
baseline requires the dependencies from its Gemfile.lock. Python 3.12 or newer
runs the harness independently of the candidate language.

```sh
python3 tests/compat/core/run.py --case version --case connection-failed-host
python3 tests/compat/core/run.py --candidate /absolute/path/to/confctl --case ls-default
python3 tests/compat/core/run.py --export-baseline /empty/oracle-directory
```

The export command prints the library root. Other compatibility packages can set
`CONFCTL_RUBY_ORACLE_ROOT` to that directory and use its `lib/` and `bin/confctl`.
Changing an expected result is deliberate: `--record` always invokes the archive,
and refuses a candidate. Use `--evidence /owned/output/directory` to retain the
raw stdout/stderr bytes in Base64, stdin, native exit/signal, ordered child calls
and final files alongside the normalized comparison. Expected files contain
normalized values; raw captures remain verification evidence. The runner never
drops an arbitrary stderr line, exception, result status or output column.

Monitoring utilities must write results outside project source. Record with
`--record --record-output /owned/evidence/expected --evidence /owned/evidence/raw`,
then replay with `--expected-dir /owned/evidence/expected`. The package owner
inspects and promotes expectations after the complete capture/replay passes.

`coverage.json` enumerates all 29 registered action leaves, arguments, aliases,
option groups/defaults and assigned cases. Refresh it with `--manifest` after
editing `cases.py`; generated coverage is not proof that a case executed. Help
cases establish static routes/options; operational cases establish only the
effects they reach. See [compatibility contract](../../../docs/compatibility.md)
for the remaining acceptance gaps and compatibility decisions.

Every invocation gets a new temporary cwd and home. The child directory takes
precedence on PATH; known Nix/Git/SSH entrypoints point at `child.py`. Unknown
requests are recorded as violations and fail the case. SSH accepts only the
fixture `alpha.invalid`, never a live target. The fake Nix implementation has
no evaluator or build daemon connection. For gen-data, the actual locked HaveAPI
client authenticates against an owned loopback mock HTTP server. A test-only
preload redirects only the Communicator constructor URL after asserting the
original vpsFree endpoint and default TLS verification. Every HTTP request and
TCP socket is confined to that mock origin; redirect and negative HTTP/socket
probes prove the guard fails closed. Synthetic credential values never enter
the HTTP event log, and protected echo fails the case. Application/client
authentication, challenge, decoding and file-writing code remains unchanged.
Other network protocols use a closed localhost proxy.
The Ruby preload cannot confine a non-Ruby HTTP client. API cases therefore
refuse `--candidate`; the Rust API owner must supply a confined adapter test
transport before candidate API equivalence is claimed. Select non-API case IDs
for current CLI differential runs.
The test supervisor kills only its owned process group after a 15-second
fake-only case deadline, then joins it. This deadline is a harness failure, not
an inferred application timeout contract.

Normalization replaces only the per-case directory, extracted source directory,
logger filename timestamp and explicit temporary-file nonces. Fixed generation
names and saved timestamps remain compared. Child calls and side effects retain
order. No timing-appendix information contract is implemented here yet. An
uninterpreted changed appendix stays UNQUALIFIED; timing meaning and live
behavior need their owning checks, not a Ruby punctuation requirement. Do not
discard stderr or change the Ruby oracle to accommodate candidate output.

GC-01 is a separate adjudicated baseline defect: Ruby crashes on `.line` after
successful child progress and on the empty-stream flush after a failed child.
Both `gc-baseline-line-error` and `gc-child-failure` retain their raw worker
tracebacks only as evidence; neither becomes an ordinary expected candidate
stderr file. Baseline replay
asserts that exact defect was reached. Candidate comparison requires the same
child boundary and success exit for successful GC. The separate failed-child
case and owning Rust tests must establish nonzero/mixed-host failure, joins and
renderer/infrastructure-error precedence under the compatibility contract.

The 372 cases comprise 370 ordinary snapshots and these two defect observations.
Run `python3 tests/compat/core/runner_test.py` for the focused harness checks:
unexpected child refusal must retain raw streams, status and effects; the GC
candidate predicate must accept success only after a successful child and require
exit 1 after a genuine fake-child failure. This test driver validates the harness
policy; Rust mixed-host, renderer-fault and joining tests remain separate gates.

Additional input qualification is independent of the 372-case manifest:

```sh
python3 tests/compat/core/qualify_inputs.py --evidence /owned/evidence/raw
python3 tests/compat/core/qualify_inputs.py --record-output /owned/evidence/expected --evidence /owned/evidence/raw
```

The eight `expected-follows` observations use the same SHA-checked Ruby archive.
Array references resolve only their first string segment, including when later
segments name a different leaf or do not exist. The confined `child_follows.py`
accepts only the exact setter lock invocation, changes that first node, and
replaces its original metadata. The real setter restores the saved metadata
before publishing; the different nested leaf remains unchanged. A missing first
node yields empty list fields and a failed set with the original log-locator
prelude, no child invocation and no lock mutation. `follows-manifest.json` binds
the source revision and reviewed snapshot checksums. `--case` selects a named
observation for a focused rerun; recording never invokes a candidate.

`qualify_login.py --evidence /owned/evidence/raw` compares ten native-byte path
observations in `expected-login-join.json`. It runs the archived `GCRoot.dir`
with an injected login and disposable per-user root, forwarding the actual Ruby
`File.join` call without changing the login bytes. It never creates that root.
Cases cover empty and nil login values, leading, embedded, trailing and repeated
slashes, dot segments and non-UTF-8 bytes. Raw output uses Base64; the normalized
expectation also stores equivalent hex so Rust tests need no new codec
dependency. These cases establish path-joining behavior only. They do not
exercise libc failure or its USER fallback.

`qualify_settings.py` uses ten separate observations in `expected-settings` to
check invocation defaults, per-Nix settings and command construction order.
The two settings replies deliberately differ, so the fixtures can distinguish
captured jobs from later settings and invocation columns from client settings.
Failures in the first or second settings child preserve exact streams, native
status and the invocation-log diagnostic. The two build observations remain
Ruby-only while the Rust build route is unavailable.

```sh
python3 tests/compat/core/qualify_settings.py --candidate /absolute/path/to/confctl-rust --case settings-bootstrap-failure --case settings-client-failure --evidence /owned/evidence/raw
```

Recording requires `--record-output` and always invokes the archived Ruby
baseline. Review the source and raw evidence before promoting new expectations;
the original 372-case manifest remains unchanged.

`qualify_channels.py` compares 20 additional full `inputs channel ls` routes in
`expected-channels`, bound by `channels-manifest.json`. These cover selection
order/duplicates, empty and array mappings, typed non-string inputs, the separate
settings lifetimes, each child failure, lock errors, operational surplus Usage
and a held cooperating writer. It checks all child events and non-log effects;
retained log names/modes are exact, while raw log bodies are saved separately.
The writer fixture asserts the existing mutation lock's device/inode/content
are unchanged; fresh roots must not create one.

```sh
python3 tests/compat/core/qualify_channels.py --candidate /absolute/path/to/confctl-rust --evidence /owned/evidence/raw
```

Only candidate comparison of `channels-corrupt-lock` selects the separate full
`expected-channels-candidate/LOCK-JSON-01.json` frame. This named decision uses
structured serde coordinates in the accepted diagnostic for the observed
invalid first object key. Its baseline hash and sole changed stderr field are
checked; actual output is never filtered. Ruby recording/replay uses the
unchanged original expectation. No wider syntax, coordinate or other I/O/scalar
diagnostic parity follows from this case.
