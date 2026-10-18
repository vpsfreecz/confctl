# Core compatibility oracle

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
order. No timing-footer exception is implemented here yet: the Rust timing
package must add a strict suffix parser with its own assertions before the
accepted stderr appendix can pass. Do not discard stderr or change the Ruby
oracle to accommodate candidate output.

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
