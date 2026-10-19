# Helper qualification drivers

These sources are unrun qualification support. They neither establish parity
nor replace the immutable Ruby helper sources and M0 captures. The declared
test environment supplies Ruby 3.4.9, JSON 2.21.2 and Net::HTTP 0.6.0. The product
backend has no Ruby/Python runtime fallback.

The two externally driven Rust fixture targets require the opt-in
`qualification-fixtures` feature. Normal `cargo test --locked` skips their
mains. Compile them without invoking their external protocols with:

```sh
cargo test --locked --features qualification-fixtures --no-run \
  --test confctl-helper-diagnostic-fixture --test confctl-watchdog-fixture
```

Do not run an unfiltered feature-enabled test suite: these mains require their
driver's explicit inputs. `diagnostic_fixture.rs` invokes the shared helper
bodies and checks that every recorded command start has a returned status.
Trace, encoding and receipt open/write/sync failures return ordinary nonzero
fixture errors. They do not park the process or publish ownership markers.
A failed or missing receipt cannot qualify a case or establish nested child
identity. Partial receipt files remain failed output for driver diagnosis.

`http_qualification.py` accepts an absolute original Ruby executable, the
compiled Rust unit-test binary, an empty owned evidence directory and either
`json` or `codec`. It serves fixed bytes on loopback HTTP, records complete raw
request headers and both processes' raw stdout/stderr/native exit/signal, and
joins its server thread. The original probe exercises the actual Net::HTTP
index/image body APIs and JSON parser; the ignored Rust test exercises the real
private client/body/parser. Neither side is a full installed helper process.

The JSON family uses the input-only seven-row syntax list, including the one
already approved bad-object case. It produces no new expected output. The codec
family records identity/gzip/zlib, chunked framing, case-insensitive/x-gzip,
Content-Range bypass, unsupported whole encoding, envelope/header mismatch,
empty wire/member, corruption/truncation, trailing bytes/multiple members and
late HTTP framing failure for both index and image bytes. Raw post-codec byte
counts are observations, not a selected trailing-data rule. A retained table
must adjudicate these results before decoder policy or wider KEXEC-03 promotion.
Proxy and CA/TLS tests need separate controlled servers and remain open; this
loopback driver deliberately removes proxy settings only in its test children.

`watchdog_fixture.rs` is a standalone, harness-free, test-only artifact. Build
its opt-in target with test compilation enabled so the named barriers and
test-only `invoke` exist. It must never be installed, linked to libtest,
or run against the host current-system link. The production helper always
passes `/run/current-system`; the fixture alone supplies its owned root's
`current-system` path on initial and self-reexec entry. Its isolated guardian
uses safe Rustix subreaper/wait calls and puts only the helper parent into a
fresh group, leaving the real worker's inherited-group/setsid path unchanged.
After spawn, reporting failures remain infrastructure failures but cannot skip
waiting. A failed direct-parent wait is retained while an exact-child wait
recovers ownership before adopted reaping begins. The guardian writes its final
result only after ECHILD; the driver keeps it alive during failure cleanup even
when a process snapshot temporarily shows no descendants.

`watchdog_qualification.py` accepts that absolute fixture binary, an empty owned
output directory and one explicit case: confirmation, timeout, broken outputs,
pre/post-detach original-group HUP or direct private-worker detach failure. The
fixture parent begins as a group leader. The worker pauses only immediately
before setsid and immediately after success. At both barriers the driver records
PID/starttime/PPID/PGID/SID, controlling-TTY identity and actual `/proc/<pid>/task`
entries. It asserts one worker task, the inherited parent group before detach,
the same worker becoming session/group leader afterward, and no deployment
effects at either barrier. Pre-detach HUP and failed setsid must never reach the
post-success barrier. The driver pins owned processes with pidfds and keeps the
subreaper alive through adopted-child waits. It controls both logging pipes, check-file
confirmation and fixture switches with more than pipe-capacity output on both
streams. Generated switch scripts are test tools and exit 7 deliberately;
the helper must retain its original ignored-native-status policy.

Test-only records are written after successful waits at the common parent and
switch wait sites. They retain owner/child PID and raw/code/signal status. The
driver matches those records to worker barriers, switch PID/starttime/effects,
switch completion after 2 MiB on each pipe, the guardian's direct-parent wait,
and each adopted native result. Normal completion requires one parent-owned
worker wait and one worker-owned wait per switch. After parent HUP, the guardian
must reap exactly the witnessed worker; any adopted switch child fails ownership.
Report I/O or recovery-wait errors fail the fixture even if reaping completes.
These records are compiled out of installed helpers and introduce no extra
product waiter, signal policy or retry.

Review the actual effect/status witnesses before accepting a case. A pre-detach
HUP can kill the worker before detach and must cause no deployment effects;
there is no source-only claim of pre-detach survival. Post-detach loss must
complete the real confirmation/rollback protocol and reap children. The receipts
do not by themselves trace every wait syscall, establish cgroup-wide survival,
or prove all failure-injection cleanup paths. The explicit group-HUP cases run
without a controlling TTY in the guardian's session and do not establish sshd or
terminal-generated signal behavior. Reporting/wait fault injection, unexpected
wait-error termination and driver containment still need native evidence. These remain explicit native
review/acceptance gates. No signal disposition or product cancellation policy
is altered to make a fixture pass.

All original/helper candidate stderr comparisons remain exact per their named
rows. The 27 unobserved error groups in the bounded inventory are unqualified.
Do not filter traceback-looking output, append inferred expectations, regenerate
frozen data or extend CARRIER-01/KEXEC-02/HELPER-DIAG-01/KEXEC-03 by error class.
