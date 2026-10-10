# Process-boundary compatibility and measurement harness

This standard-library Go module drives installed executables. The immutable Ruby
oracle is revision `cc40679d267165aecfa569128438bb55fe910268`; site scripts are
`ae670dc0d4a5d43adf9560da1a6a0a35925cd2e5`. Expected observations are copied only
from original Ruby captures after provenance review. The driver never generates
or updates expectations from Go. Source contracts and unobserved families are
listed in [the catalogue](../../docs/compatibility/README.md).

Build `.#confctl-compat-tools` or run `go test ./...` and `go vet ./...` in the
experimental Nix shell. Package checks run the whole module, including the real
Linux PTY and descendant cancellation tests. Race checks are separate. No
integration or benchmark success is implied by a package build.

`package.nix` calls the unchanged original package function and shallowly
substitutes only Git/OpenSSH/Nix packages. Named tool aliases are in `$out/bin`,
which the original wrapper prepends. An outer PATH substitution does not replace
those fixed tool paths. `experiments/confctl-go/fixture-package.nix` provides the
same substitution for the candidate and requires all four inputs: `pkgs`,
`fixtureTools`, `sitePackage` and `registryTemplate`. Supply the configuration-owned
site package and its private fixture template explicitly. Only this composition
installs `share/confctl-go-prototype/registry.json`, substituting the supplied
site package for `@SITE@`; the normal Go package provides only the CLI and hook
driver. Standard packages are reserved for the real tier. The test hook driver
retains the oracle's Ruby/deps/source without runtime monkey patches or native
deployment.

A case declares source references, literal CLI argv, environment, stdin, TTY,
initial files, semantic tool/host/argv/stdin/occurrence response rules, a driver
deadline, raw causal constraints and explicit normalizers. `read_stdin` is set
only on requests whose original caller supplies input and EOF (such as status's
bash script). Other calls probe and record empty-open versus empty-EOF topology;
they do not block waiting for an EOF the original caller never sends. Plans are
compiled before command timing into compact per-tool/host/installable files.
Unknown calls fail even with diagnostic tracing disabled. Fixture dispatcher
startup/JSON/validation cost remains artificial external work.

Raw evidence records actual executable/hash, pinned contract source, actual
candidate revision marker, exact fixture-file SHA256, raw stdout/stderr/exit,
file/symlink modes and contents, Git state when explicitly enabled, external
argv/stdin/context/results/occurrences and process start/end. Candidate revision
and package/source/toolchain manifests are distinct from oracle provenance.
CPU/RSS labels refer to Linux child rusage, not aggregate live tree memory.
Three PTYs preserve true tty descriptors and stream separation; pipe captures
are not declared equivalent to TTY captures. Driver cancellation tracks proven
PID/start-time descendants across separate process groups and reaps them on
every capture path. Live descendant work after a successful root exit is a
capture failure, with bounded cleanup; PTY draining remains under the capture
context after root exit. The declared driver deadline is test containment,
separate from candidate command policy.

Normalization is per field. Configuration and capture roots are distinct;
state bytes, modes, symlink paths, store hashes, statuses and signals are never
globally erased. The log allowlist covers only matching command-log filenames,
consistent bijections for generated eight-hex command IDs and exact
`GLI::Command::ParentKey:0x...` tags, known process-duration fields and the
configuration root. Log text/order, argv, results and unrelated hexadecimal
strings remain exact. UUID mappings retain repetition and distinguish IDs.

Concurrent SSH comparison requires raw global setup-Nix-before-SSH barriers and
explicit per-machine/profile sequential flows. Every SSH event belongs to one
flow; identical shared-host uptime calls cannot satisfy two flows. Once those
raw checks pass, only the SSH suffix is canonicalized by literal host/argv/stdin/
occurrence. Nix chronology and exact UI/file/log array order remain observable.
A reversed call within a profile fails. No whole-transcript sort is allowed.

Commands below are prepared interfaces; recorded evidence determines which
checks have actually passed:

```sh
COMPAT_SITE_SOURCE=/tmp/2026-10-09-evaluate-confctl-rewrite-language-source/vpsfree-cz-configuration \
  compat --observe --cases 'tests/compat/fixtures/*.json' \
  --binary "$fixture_oracle/bin/confctl" --tools "$fixture_tools/bin" \
  --hook-driver "$oracle_hooks/bin/oracle-hook-driver" --evidence "$new_capture_directory"

COMPAT_SITE_SOURCE=/tmp/2026-10-09-evaluate-confctl-rewrite-language-source/vpsfree-cz-configuration \
  compat --cases 'tests/compat/fixtures/*.json' \
  --binary "$fixture_native/bin/confctl-go-prototype" --tools "$fixture_tools/bin" \
  --registry "$fixture_native/share/confctl-go-prototype/registry.json" \
  --hook-driver "$fixture_native/bin/hook-driver" --evidence "$new_comparison_directory"

compat-bench --ruby "$fixture_oracle/bin/confctl" --go "$fixture_native/bin/confctl-go-prototype" \
  --ruby-hooks "$oracle_hooks/bin/oracle-hook-driver" --go-hooks "$fixture_native/bin/hook-driver" \
  --registry "$fixture_native/share/confctl-go-prototype/registry.json" --tools "$fixture_tools/bin" \
  --candidate-revision "$actual_revision" --candidate-source-hash "$actual_source_digest" \
  --samples 30 --runs 2 --scales 1,10,100,1000 --cache warm,fresh-application-cache \
  --output "$new_measurement_directory"
```

Benchmark cells include plain/script-registered help/version, ls full/exact/
filtered/custom columns, query-only status and both extension workflows. Version
exit 1 is asserted as intentional. Each cell has an instrumented causal/process
parity gate, three warmup pairs and randomized measured pair order. Timing has
fixture diagnostic tracing disabled but retains fixture request validation.
The recursive process-ownership observer remains active every 5ms. Timed wall
includes command start/wait, observer start/finish, concurrent procfs sampling
and cleanup; fixture preparation, state snapshots and normalization are outside
that interval. Child rusage excludes the controller CPU spent on this observer.
These costs depend on process topology and must be considered when interpreting
startup and scale differences. Wall minus child CPU or external span is not a
measure of removable local work. Mutation input bytes
reset between invocations while eligible application cache remains. Every sample,
failure and raw capture is retained; failed pairs enter no accepted distribution.
Summaries include absolute medians/p95 and paired bootstrap differences. Use
`--cells`, `--scales`, `--cache`, and `--runs` for bounded independent commands;
at least 30 pairs per selected cell and two independent runs are required before
recommendations. Fresh application cache does not mean cold OS page cache.
Resource/affinity/cgroup/load snapshots bracket sessions and cells. No removable
local fraction is inferred from the first-external-start-to-last-end span.

[real-local](real-local/README.md) contains the separate rootless image tier.
Runtime provisioning and benchmarks follow committed deliverables, quick checks
and independent review. This package does not deploy, merge, run GC, change
normal package defaults, migrate hooks or replace Ruby service/node helpers.
