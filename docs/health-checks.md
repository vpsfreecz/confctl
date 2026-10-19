# Health checks

`confctl-rust health-check [options] [machine-pattern]` runs configured health
checks on selected managed, runnable machines. Carried machines, null targets
and hosts without active checks are excluded. Selection keeps the usual machine
name/key, pattern, attribute and tag rules. An empty eligible selection fails.
The confirmation shows per-host counts and the total; `--yes` bypasses that
confirmation. `--max-jobs` must be positive and defaults to five concurrent
logical checks.

## Configured checks

Checks keep their metadata type and list order. `builderCommands` run local
argv as the invoking user, without an implicit shell. `machineCommands` use the
selected machine: localhost runs directly, other targets use root SSH with the
configured port and nonempty `CONFCTL_SSH_CONFIG`. Each argument remains one
native argument. Attribute placeholders expand once using Ruby-compatible
name/key aliases and dotted metadata values. Missing attributes become empty;
`{checks}` is the number already collected for that host at that point. A
replacement containing another placeholder is not expanded again. Empty argv,
unknown check types and malformed active settings fail configuration.

Command checks compare the configured `exitStatus`, including an expected
nonzero code. Signals are distinct from exit codes. Stdout and stderr stay
separate raw byte streams, without trimming or sanitizing before comparison.
The first output failure uses this precedence: stdout exact `match`, first
missing `include`, first present `exclude`, then the same rules for stderr.
An exit mismatch and that first output mismatch can both be reported. Raw
captures remain unchanged by display formatting.

Enabled systemd checks apply only to NixOS hosts. A nonempty system-property
group runs `systemctl show`; each nonempty unit group runs `systemctl show`
with one literal unit or pattern argument. Each group uses its shortest timeout
and shortest cooldown. Empty groups add no check. Property records split at the
first `=`, preserve value whitespace and additional equals, and use the last
value for duplicate keys. Missing and mismatching properties are reported in
configured order. Native nonzero or signal results fail before property checks.

## Attempts, results and logs

One task represents a logical check across its attempts. Checks enter through
the existing process owner, with separate captured and forwarded streams,
host/check descriptions, attempt diagnostics, log locators and terminal results.
Ordinary failed checks let the rest of the batch finish. The summary returns
nonzero if any check remains failed. Spawn, capture, delivery or cancellation
errors stop admission and join entered children; unrun checks never pass.
Already delivered task results remain actual observations.

The first attempt always runs, including with timeout zero. After a completed
failed attempt, confctl reports it, waits the configured cooldown interruptibly,
and retries only while monotonic elapsed time is at most the timeout. The
retry budget does not kill a running child. A later passing attempt succeeds
the logical check while retaining earlier failure diagnostics.

Every completed failed assessment requests the existing invocation log be kept
before the next diagnostic or cooldown, even if a later retry succeeds. This
includes exit-zero output mismatches and systemd property failures. An expected
nonzero result with matching output does not request retention. Retention is
sticky and does not change task results or exit status; it uses the existing
checked flush, sync, close and path verification. A keep request alone performs
no I/O and promises no durability. A first-attempt successful invocation without
a keep request removes its log. Finalization failure still fails the invocation.

## Effects and limits

Configured programs can have effects locally or remotely. Earlier effects remain
after failure or cancellation; confctl does not undo them. The route itself
changes no generation, profile, kernel JSON, deployment state or mutation lock,
and performs no target-system build, copy, activation, rollback or reboot.
Inventory evaluation/materialization and invocation logging still occur.
Existing metadata and target tools remain the interface without a format
migration or installed helper. After correcting a service or configuration,
run health-check explicitly again. Passing checks do not establish deployment
or rollback safety. Saved test activation and its deployment retry policy remain
unavailable; the default `confctl` entrypoint remains Ruby. Fake-tool command
tests do not qualify real services, hosts or full live-terminal behavior.
