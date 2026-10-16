# Health checks

The Go health runner checks command output and systemd properties. Its core
adapter prepares checks from evaluated machine metadata and executes them
through the existing process and SSH argument adapters. `Engine.HealthCheck`
supplies confirmation, progress and the final summary for the public
`health-check` command. Logs remain available after a failed attempt, including
a check that later succeeds.
The runner does not activate configurations or retry deployment operations.

## Selection and metadata

Planning applies the existing machine pattern, attribute and tag selectors,
then keeps managed machines with a target that are not carried by another
machine. It excludes machines without checks. An empty result returns
`No runnable machines to check or no health checks configured` before any
confirmation or command execution.

Checks retain machine order, the order of health-check types in the original
JSON object and the order of systemd unit groups. The adapter requires the raw
evaluated metadata; it does not reconstruct object order from a Go map. The
existing machine decoder's `_module` removal also applies here. Missing or
malformed evaluated fields return a `health.SchemaError` with their source
path. Unknown health-check types and empty systemd unit groups are errors.

The supported types are:

- `builderCommands`: commands executed locally, in the configuration directory,
  with the caller's environment.
- `machineCommands`: commands executed on a runnable target using the existing
  SSH arguments, or locally when that target is `localhost`.
- `systemd`: when enabled on a NixOS machine, one manager-property check for a
  nonempty `systemProperties` list, followed by each `unitProperties` group.
  Each group runs `systemctl show`, with its unit pattern as one literal
  argument when present.

The Nix module supplies command defaults: empty description, expected exit
status zero, timeout 60 seconds, cooldown 3 seconds, null output matches and
empty include/exclude lists. Property checks use the same timeout and cooldown
defaults. Systemd checks default to enabled, with `SystemState=running` as the
manager property and no unit groups. The adapter consumes these evaluated
values; it does not fill in absent fields to conceal malformed metadata.

Every command argv element substitutes `{attribute}` independently. Attribute
access uses the existing machine view, including dotted names, `name`, the
`key`/`machineKey`/`flakeKey` aliases and Ruby-compatible scalar or compound
string representations. An absent value becomes an empty string. `{checks}`
uses the number of checks already constructed for that machine, before the
current command is appended. Substitution performs no shell expansion. An
empty description uses the shell-joined argv; a whitespace-only description
remains explicit text.

## Command predicates

Captured stdout and stderr stay separate and retain all bytes, including
newlines and empty strings. The runner first records an unexpected exit status,
then at most one output failure, in this order:

1. Stdout exact match, include fragments, exclude fragments.
2. Stderr exact match, include fragments, exclude fragments.

`match` compares the whole byte string. Null disables it; an empty match requires
empty output. Include and exclude lists retain their configured order and
report the first offending fragment. An empty exclude fragment is present in
every string and produces a failure with that empty fragment intact. Output is
never trimmed or interpreted as a regular expression.

A normal nonzero child status, including SSH status 255, is data for the
configured expected status. Spawn, wait and cancellation errors stop the
operation and return partial results. A child without an exit status also
returns an operation error; it is not counted as a passed check.

## Systemd properties

A nonzero `systemctl` status produces its command failure before property
parsing. Successful output splits on LF and parses each line at its first `=`.
Lines without `=` are ignored. Property names and values retain whitespace and
CR; later duplicates replace earlier values. The Ruby source's leading-`=`
slice rule is preserved: `=x` has the property name `=x` and value `x`, rather
than an empty property name.

A missing property differs from a present empty value. Failures follow the
configured property order. A group's timeout and cooldown are independently
the minimum values among its property checks, including zero.

## Attempts, concurrency and failure results

Each check records its wall-clock start and runs a first attempt even when its
timeout is zero. Each attempt clears the preceding predicate failures. After a
failed attempt, the runner reports that failure and waits the configured
cooldown. It then stops retrying only when `start + timeout < now`; equality
allows another attempt. Timeout is checked between attempts and imposes no
child deadline. A successful command can therefore run longer than the timeout.
Zero cooldown remains zero, and there is no added retry limit.

Worker requests use arbitrary-precision integers. The Go adapter rejects
`max-jobs <= 0` before inventory loading, planning, confirmation or execution.
For positive requests, the runner takes the minimum of the requested count and
the actual number of checks before converting to a native worker count. This
corrects the source's zero-worker path, which could report unrun checks passed;
it introduces no global worker cap.

Predicate failures finish their own retry loops and do not cancel unrelated
checks. Process errors and cancellation stop further scheduling, cancel owned
running callbacks and join them before returning. Canceling an SSH child does
not prove that its remote command stopped. Cooldown waits are interruptible.
Timeout and cooldown seconds retain their integer values without overflowing a
native duration; long waits are split into representable timer intervals.

Results retain input identity and order, attempt count, latest predicate
failures, operation errors and whether any attempt failed. Unstarted or
interrupted checks are never passed checks. Progress callbacks report each
check's start, failed attempts and normal completion; callbacks are serialized.

The command retains the log after any failed attempt, even if a later attempt
succeeds. The adapter reports this through `HealthResult.RetainLog`. Public
dispatch passes `!result.RetainLog` to `Engine.CloseLog` on success and retains
the log on error. Use the retained log to diagnose failures; the runner does
not repair machines or retry deployment.

## Standalone command adapter

`Engine.HealthCheck(inv, in, out)` uses the existing `health-check` option and
argument declarations. It validates workers and selects checks before asking
for confirmation. Unless `--yes` is set, it shows the configured machine
columns with a checks column and total, then uses the existing yes/no/EOF
confirmation behavior. A refusal runs no checks.

Progress uses serialized lines on terminals and redirected output. Each line
identifies the check and machine, with failed attempts reported before the
final result. The final summary groups failures in input machine and check
order, regardless of worker completion order. Spawn errors and cancellation
leave interrupted or unstarted checks marked incomplete, never passed.

Any final failed check returns `Health checks failed`; all passed checks return
success. A successful retry still requires log retention. The adapter writes
progress and child output to the existing invocation log and returns output
or log failures. It owns no terminal display, signal handler or separate
process runner.

## Source and verification boundaries

The contract follows `lib/confctl/health_checks`, `Machine#health_checks`, the
cluster CLI health-check methods and `nix/modules/cluster/default.nix`.
Deterministic Go tests use synthetic evaluated metadata, fake clock/executor
callbacks and disposable nix, local and SSH executables. They check selection,
confirmation, ordering, predicates, property parsing, retry boundaries, child
startup and cancellation, partial results and actual log retention/removal.
Public CLI fixtures exercise the actual Main entrypoint, exit statuses and log
retention with disposable tools, including a successful retry. These checks do
not establish fleet health, actual systemd behavior or installed-package behavior.
