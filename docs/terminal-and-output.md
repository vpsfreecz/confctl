# Terminal and output ownership

The Rust implementation uses one acknowledged event consumer for confctl output
and one input broker for operator answers. Operations send typed events. Child
captures and command logs preserve bytes and stream identity; only live display
text is sanitized. A task's result comes from its operation, independently of
progress counters or parsed Nix activity.

## Plain formatting and live view

Plain tables preserve the Ruby formatter's character-count padding, two-space
column separators, unpadded final column, row-label indentation and multiline
values. `TableFormat` makes layout, header and alignment explicit. Automatic
layout uses rows for zero or one object and columns for multiple objects. This
padding counts Unicode characters; live clipping uses display width and
grapheme boundaries.

Typed per-cell styles are separate from layout. Missing entries use `Plain`; excess
rows or columns are rejected before output. Widths use the original cell text.
Padding stays outside the styled value and reset, including right alignment and
multiline values; headers, row labels and continuation indentation stay plain.
Diagnostics can carry ordered styled parts for mixed lines, such as a heading
and log path or a machine and generation. The consumer formats and delivers the
whole line atomically. Without color, those parts retain the original text,
spacing and stream. Headings are bold, generations and log paths cyan, operation
and skip notices yellow, success green and failure red.

The live model keeps task registration order, explicit final results, bounded
sanitized tail lines and full log locators. Visible membership prioritizes failed,
incomplete and canceled work before restoring registration order. Known counts
remain visible; valid positive totals permit a progress bar. Narrow rows keep
the result before a clipped description. Rendering reads elapsed time without
freezing the timing ledger.

The full live panel is used by `build` and `deploy`, and during the build phase
of other commands such as `status`. Other phases keep ordinary output and use
a single transient command spinner on stderr. A shared build phase enters the
full panel before preparation and leaves it before the built-generation report;
the enclosing command then continues with its ordinary presentation.

In the full panel, Preparing and unknown build counts use a stationary ASCII
spinner (`|`, `/`, `-`, `\`) until measurements arrive. Measured progress uses
the `=` bars. The spinner indicates activity without claiming that a child has
started or inventing completed work, percentages or remaining time. It pauses
during prompts and foreground children and stops when the task finishes.

`TaskPending` records admitted work whose execution body has not been entered.
`TaskStarted` promotes the same ID and description to running logical work, or
registers it directly when there is no pending phase. Running does not assert
that a native child is alive.
Pending work may finish as skipped, canceled, failed or incomplete before entry;
the view retains its not-started status. Progress requires a running task, and
only a task that has entered execution can succeed. Repeated identical results
are harmless; conflicting results and backward transitions are delivery errors.
When an output group ends without a task result, the final view shows incomplete
with its last observed entry status. It does not infer success or cancellation.

[`init`](config-init.md) needs no existing flake, Nix child or consent. It emits
acknowledged `mkdir`/`mkfile` notices before creating each literal starter
template, with a cancellation check after acknowledgement. The existing
advisory mutation lock spans the writes; Ruby writers do not share it. A conflict,
write/output failure or cancellation retains completed and partially written
files. These local effects admit no host or native-process tasks.

[`add`, `rename` and `rediscover`](configuration-commands.md) emit acknowledged
notices before local edits and check cancellation after acknowledgement. They
hold one advisory mutation lock through core edits, subsequent settings reads and
enabled netboot publication. Settings are required even when publication is
disabled; a later failure retains completed edits. These commands need no
confirmation and contact no hosts. The guide covers migration and recovery from
partial effects.

[Input mutations](flake-inputs.md#rust-input-mutations-and-recovery) use one logical
task (ID 0) for selection, serial Nix edits, optional history and Git commit. Nix
mutation streams are forwarded and captured. Git commit parks presentation and
inherits streams through the foreground owner; its editor follows actual stdout
TTY status. A task result includes joined completion and terminal restoration;
failures retain earlier lock/cache/commit effects.

[`gen-data vpsadmin`](gen-data.md) uses one logical task under the mutation guard
through authentication, observation and complete-file publication. User name,
protected password and described challenges use the existing input broker;
credentials, challenge values and tokens stay out of diagnostics and logs.
Generated contents are not printed. `all` publishes containers before networks,
so a later failure can retain the first replacement. The guide describes the
same-origin and no-replay rules, server validation limits and explicit recovery.
Loopback HTTP, fake Nix and PTY tests do not qualify live credentials, TLS or site
behavior.

`runtime-kernels update` emits pending events for the confirmed, validated host
probes and starts each task as its logical probe body enters the process runner.
Each host has its own task ID and description. A decoded kernel succeeds; an
ordinary host error fails that task while allowing the refresh to finish and
remove the host's saved kernel. Infrastructure errors and cancellation fail the
command after joining entered probes. Probe results precede the separate atomic
save of combined state; successful tasks do not promise that save succeeded.
Declining confirmation admits no probes. Controlled command-route tests do not
establish live progress, quiet elapsed updates, resize or full terminal qualification.

`test-connection` admits one task per confirmed runnable host. Each serial uptime
probe starts and finishes its own task. Ordinary host failures fail those tasks
while allowing the completed command to exit 0. Local generation listing emits
a result table without host-probe tasks. Remote or combined generation listing
admits a task for each selected host query; unavailable hosts fail their tasks
while ordinary host failures still allow the listing to exit 0.

[`generation rm` and `generation rotate`](generation-cleanup.md) hold the
cooperative mutation lock from planning through consent, removal and optional
GC joins. Local record and GC-root unlinks, profile deletion and store GC are real effects;
failures retain completed work without current-link repair or rollback. The
guide describes retention and rotation's GC OR policy.

GC forwards both streams live with host labels while raw captures and logs retain
native bytes. Only recognized deleting-path lines count; the total remains
unknown and native outcomes decide success. Ordinary host failures aggregate;
infrastructure, delivery and cancellation errors stop admission and join entered
children and relays. The cleanup guide describes the progress and concurrency
corrections relative to Ruby.

`status` with an explicit generation admits one task per confirmed, resolved
host observation. Saved status compares the saved toplevel and input metadata
with deployed state; `--generation none` evaluates current target inputs instead.
Both modes read deployed uptime, profiles and inputs; saved status also reads
the running system or carried profile path. Missing saved targets admit no task.
Failed host observations fail their tasks and leave offline or unknown table
cells; ordinary host failures still allow the completed command to exit 0.
Cancellation and delivery failures join entered observations. Default status
first asks to BUILD and check, runs the existing build-group tasks under one
guard, then admits observations in selected order with IDs above the build range.
Returned objects supply target paths; current configured inputs are compared
even for reused records. Saved/none modes retain their read-only behavior.

[`diff` and `changelog`](changes.md) admit sequential host-comparison tasks for
uptime and input reads, without system builds or profile enumeration. Changelog
also runs Git mirror clone/fetch/log operations in role order. Host outcomes
remain distinct from input equality. The lazy [pager](pager.md) owns one separate
task and is always joined, including after producer errors or actual pipe EPIPE.
EPIPE stops later host and Git work; earlier observations and cache effects remain.
Disabled paging emits report bytes through acknowledged stdout, including
redirected output. Configured pager commands run without discovery, probes,
argument stripping or fallback. Ordinary host/Git failures and ordinary pager
exits can leave failed tasks while the invocation exits 0; infrastructure,
delivery and cancellation errors fail the invocation. Targeted fake-tool/PTY
tests do not establish real-site or full terminal qualification.

`ssh` admits per-host tasks and reports actual completed and failed host counts
for commands. Command stdout and stderr share one native pipe and capture.
Interactive shells park painting through the foreground terminal owner and
restore terminal and input ownership on return. Ordinary nonzero child results
fail their tasks while allowing the completed command to exit 0.

[`cssh`](cssh.md) admits one task for the single `nix-shell` ClusterSSH launcher.
It forwards and captures stdout and stderr and retains the invocation log.
The child inherits the display and authentication environment with `shellHook`
removed. An ordinary nonzero exit or signal fails the launcher task while the
completed invocation exits 0. Startup, capture and delivery errors fail the
invocation; cancellation joins the owned launcher and exits 130. Its result
does not prove connections to individual hosts or closure of windows and remote
shells that outlive it. Fake-tool route tests do not establish real GUI or host
qualification.

`build` registers one task per actual build group after consent and planning.
Groups run serially; a shared Nix build does not prove that each host executed
separately. Live builds request Nix's structured activity output while stdout
stays captured for JSON decoding. The relay displays decoded messages and retains
the original stderr in captures and logs. Plain builds keep the existing human
output and command arguments.

Building, Fetching and Total have aligned count, percentage and bar columns.
Counts describe completed work reported by Nix, with running and failed work kept
separate. Fetching counts copied or substituted paths, not downloaded bytes.
Total combines both categories only when both are known. While running, unknown
counts show `waiting for counts`; unknown, zero or inconsistent totals show `--%`.
Expected work can increase as Nix discovers more work, so percentages can decrease.
When Nix stops an activity, its completed and failed counts remain, but its unused
estimate retires. Other active activities can still keep the expected total higher.
Stopping an activity never adds completed work. Failed counts remain independent
of the completed/expected ratio and suppress the percentage rather than displaying
a misleading 100%. These percentages describe known work rather than elapsed or
remaining time. Counters reset for each native attempt; starting or stopping an
activity alone does not prove successful completion. If Nix sends no final update,
the bars retain the last observed counts.

A full bar does not finish a group: successful completion also includes generation
reuse/publication. After a group's final result, a compact snapshot with its
actual Building, Fetching and Total bars stays in terminal scrollback before
the next group proceeds. Final bars stop animating and retain actual counts;
a failed or canceled group never gains fabricated completion. The repeated live
label shows the group number and machine count; selection confirmation and generation reports
retain the complete machine list. On failure, later groups are skipped or canceled,
and earlier saved generations and partial roots remain in place. Task log locators
refer to the app-owned invocation log. Successful finalization removes that log
unless the operation requests retention; failures retain it under the existing
policy.

Saved-generation copy-only deploys admit one task per resolved host, with bounded
copying-count/path progress from native stderr and unknown totals when absent.
Optional runtime-kernel refresh uses distinct preceding task IDs. Copy results do
not claim activation. Saved-generation boot, dry-activate, test and switch add
bounded status-query tasks, copy tasks and standalone activation tasks. Activation covers
a preliminary dry run when requested and the requested action; boot and switch
set the standalone profile after successful activation. Carried switch has one
profile/callback task labelled for guest and carrier; other carried actions
have only the copy effect. Delivered task results remain actual observations if
later output or cancellation fails the invocation. Saved test and switch
activation tasks report direct execution or the existing rollback helper
protocol, without claiming its hidden inner switch status. After joined
activation and required profile setting, eligible configured checks use the
existing health runner. Each
deployment retry batch gets fresh IDs above all reserved activation IDs; attempts
within a batch share their task. Earlier failed tasks and raw logs stay retained
when a later batch passes or failures are explicitly accepted. Declined health
rounds remain visibly unverified.
New-build deploy first completes every build group, retaining the actual returned
records and publication effects, before these status/copy/activation/health
phases. IDs are reserved across all phases. Build failure admits no later phase,
and copy-only builds make no status, activation or health request. Boot with
`--reboot` reserves distinct reboot/wait IDs before health IDs. Each actual
request has one task, a log locator and live elapsed/remaining progress while
polls block. Native nowait failure fails its task while retaining the command's
continuation policy; a lower-uptime observation succeeds without proving the
request caused it or the target generation booted. Cancellation and output
failure join entered local children and preserve delivered results. Other
operation producers are not wired to this lifecycle yet. Controlled route
fixtures do not establish real-site activation or full live-terminal qualification.

`health-check` admits one task per configured logical check and starts it on
entry to the bounded check runner. Attempts share that task and its log locator;
failed attempts remain visible even when a retry succeeds. A failed assessment
requests retention of the raw invocation log before diagnostics or cooldown.
The final summary reports actual passed and failed checks. Cancellation and
delivery failures join entered work and preserve already delivered results.

The inline panel reserves a stable height from its first paint. Unused rows stay
inside the panel, so incoming output does not move the header, separators or
progress rows. Resize changes the available geometry. The header starts on the
first owned row without reserving two empty terminal rows above it.

Completed build groups and machine deployments leave compact snapshots in normal
terminal scrollback, without retaining the live log tail or unused viewport rows.
A machine snapshot follows its final requested copy,
activation, reboot/wait and health decision; copy-only and unverified outcomes
remain labelled accordingly. Bulk deployment keeps its existing execution order,
and shared health retries finish before machine snapshots are emitted. Snapshots
preserve other running work and never infer a result from a progress bar or an
intermediate task. A failed or canceled unit retains its known partial outcome.
Plain mode adds no checkpoint output. Prompts and foreground children defer
snapshots through the existing required-output owner. The next active unit may
open another live panel. Completion clears only the owned panel and retains
remaining terminal summaries compactly; required result output does not reopen
an empty panel. A finished build leaves its completed bars, built generations
and timing together above the prompt, with no padded final viewport.

The live header shows the invocation, current operation, local start time and
whole-second elapsed time. The log locator has its own wrapped line, using a
configuration-relative path when possible. Horizontal separators bound the live
tail without adding vertical characters to copied output. A tiny terminal may
show the full locator in normal scrollback when it cannot fit in the panel.

In the full panel, native activity updates the sanitized tail without also
printing a raw transcript outside the view. Other modes forward activity bytes on
their original streams and retains the existing host-attributed copy-progress
lines on stdout. Those derived copy lines use a plain-only event, so they do not
duplicate native activity in the live tail. Reports, tables, diagnostics and
prompt frames keep their required-delivery behavior. Raw process captures and
invocation logs remain independent of the bounded display; completion and cancellation do not replay
the live activity history.

Single-command output has no source prefix. Mixed sources use readable command
or host labels so interleaved fragments remain attributable. Source changes keep
partial lines separate. Task IDs and stream identities remain internal metadata;
registering several pending serial tasks does not make their output concurrent.
Tail text and source metadata remain bounded.

The presented process runner announces attempted commands before spawning them
and before any child output. An accepted announcement does not assert that spawn
succeeded. Quiet startup and settings commands can therefore show activity before
any output, when terminal ownership and line boundaries permit.
Audited producers supply a shell-readable, redacted display; other commands show
their safe description with arguments omitted. The display never derives from
arbitrary environment values or stdin, and native argv remains unchanged. Plain
mode adds no command announcement to stdout. Foreground commands keep exclusive
terminal ownership.

Log paths keep one safe display encoding: backslashes and quotes are escaped;
LF, CR and tab use visible escapes; other ASCII controls and non-UTF8 bytes use
byte escapes. Unicode controls, line separators and directional controls are
escaped, while printable wide and combining text remains readable. Wrapping
changes only the display; native PathBuf values remain unchanged. Successful
logs are removed unless an operation already requests retention. Failure and
cancellation retain logs under the existing policy, and final locators refer to
retained evidence rather than a deleted successful log.

Both output streams must be usable terminals for a full panel. Without that
capability, commands can still show the compact spinner when stderr alone is a
usable terminal. Thus `confctl ls >file` keeps stdout clean while showing activity
on terminal stderr. Redirected stderr, `TERM=dumb` or `CONFCTL_TTY=0` disables all
transient presentation. Color policy is separate: `always` forces confctl styling,
`never` disables it, and `auto` follows the destination and `NO_COLOR`. Child data
and captured paths are not recolored or changed.

The compact spinner owns only one stderr line containing the safe current command
and an additional-command count when needed. Each native attempt has a distinct
identity, even when several attempts share a logical task. Its completion clears
the spinner after output has drained and terminal ownership has been restored;
that notification does not imply success. Capture-only results remain private.
Before any visible output, the owned spinner line is erased and suppressed for
that child. Ambiguous concurrent output may suppress other active spinners too;
completion does not resurrect them. Partial lines and uncertain cursor positions
suppress transient output rather than adding newlines or overwriting real output.
Prompts and foreground children clear and park the spinner before taking ownership.

## Why the painter uses relative output commands

Ratatui widgets render into a `Buffer`. The writer paints it with output-only
Crossterm move/clear commands in the normal screen. It establishes a known line
anchor through its own output, reserves a terminal column to avoid soft wrapping,
and invalidates its geometry after resize or terminal handoff. It compares the
previous and current buffers, writes changed rows in one batch and skips identical
frames. Activity and stationary spinner updates coalesce to at most ten paints per
second; elapsed text changes once per displayed second. Prompts and foreground
children park painting, and terminal results stop the animation.
The live frame pads unused rows inside its fixed viewport while work runs;
completed snapshots omit that padding. A fresh anchor
adds a newline when required output ended mid-line or a foreground child may have
left a partial line. It never moves into unowned history to remove blank space,
asks the terminal for cursor coordinates, enters an alternate screen, changes
raw mode or reads keys. Resize comes from SIGWINCH and terminal-size ioctl.

The locked dependencies explain this choice:

- `ratatui-core 0.1.2`, `src/terminal/init.rs:122` onwards: inline initialization
  computes its viewport using the backend cursor position.
- `ratatui-crossterm 0.1.2`, `src/lib.rs:302`: cursor-position lookup delegates to
  `crossterm::cursor::position()`.
- `crossterm 0.29.0`, `src/cursor/sys/unix.rs:25-42`: that lookup can enable raw
  mode, send CSI 6n and consume terminal input while looking for the response.

Using that constructor would add another input owner and could consume an
operator's typeahead. Supplying invented cursor coordinates would hide the
conflict without establishing a safe viewport. A backend change can be
reconsidered if it provides an inline anchor without reading input or mutating
terminal input mode.

## Prompt and child ownership

`Idle`, `Live`, `Prompt` and `ExternalChild` are terminal ownership states.
Prompt or child acquisition requires an acknowledged park/flush barrier. Prompt
frames carry their preview and text explicitly, so the consumer can distinguish
them from background diagnostics without guessing from their content. The
input broker remains confctl's only reader and does not read ahead into a later
prompt or child.

The broker reserves a protected read and disables terminal echo before any prompt
bytes are written. It starts reading only after the presentation consumer
acknowledges the prompt, so queued input cannot complete the read and restore echo
before the prompt appears. If the prompt is cancelled, dropped or cannot be
delivered, the broker attempts to flush protected terminal input and restore the
saved modes before releasing ownership. Non-terminal input uses the same
acknowledgment barrier without changing terminal modes.

An `OperationGroup` carries a concrete table/diagnostic prelude. The consumer
validates the whole prelude before deduplicating a group and acknowledges its
first delivery only after required bytes have transferred to output ownership.
The same restrictions apply to a prompt preview; neither frame runs callbacks.

While parked, required background bytes transfer to owned deferred spools before
acknowledgement. This acknowledgement transfers delivery responsibility to the
consumer; it does not claim the bytes have reached the terminal or survived
power loss. Required bytes retain order within each stream and must replay and
flush before checked presentation shutdown can succeed. A consumer failure
cancels application work even if an earlier producer received an acknowledgement.

A replay or spool failure retains `PendingOutput`: both owned captures,
confirmed per-stream replay offsets and any verified durable locator. A failed
append also retains the exact current chunk with its stream and pre-append spool
offset. The append may have written a prefix; that overlap metadata prevents
blind concatenation or duplicate automatic replay. Debug output reports sizes
and paths without captured contents. The application retains evidence before
dropping its temporary owners and prints only a locator successfully retained.

The private presentation control owns a sticky failure slot. The loop returns a
failure notification; checked finish transfers the actual source and owned
evidence after cleanup completes. The app identifies that notification by its
task, substitutes the retained error and preserves an earlier operation failure.
Normal shutdown requests finish before joining the loop. Failure or abort joins
first, then checks finish. Dropping a finish waiter or the loop's result does not
take the evidence. A synchronous loop `Drop` retains outstanding spools and marks
cleanup done; repeated or concurrent finish calls cannot turn failure into
success. Evidence may be lost if every owner is dropped or the process exits.

Controlling-TTY job control depends on the input terminal, including when stdout
or stderr is redirected. Attachment saves foreground process group and termios,
then hands the controlling terminal to the supervisor-owned child group. The
small synchronous mask guard adds only SIGTTOU, restores the exact old mask on
the same thread, and spans no await or spawn. It uses safe nix signal-mask and
Rustix terminal APIs. Explicit restoration reports ordered errors; Drop retains
unobserved cleanup errors for checked shutdown.

Only the supervisor consumes child stop/continue notifications and final exit
status. Stop notifications exclude EXITED; an exit observation uses WNOWAIT,
and exactly one final Tokio wait reaps the child after numeric group signalling
has been disarmed. A notifications-only ECHILD can mean the leader is an owned,
unreaped zombie. Use the separate EXITED/WNOWAIT probe to establish exit ownership;
notification ECHILD alone does not establish ownership loss. Genuine ownership
loss disables subsequent numeric signals. An ordinary leader exit does not kill
legitimate descendant tail writers.

There is one nonblocking stop/continue probe immediately after initial successful
attachment, before the worker's first async yield. Only a SIGTTIN/SIGTTOU already
observable in that probe may receive one continuation after child ownership and
foreground checks. The allowance ends even when the probe reports nothing.
Every SIGTSTP and every later stop uses the app suspension path. The narrow
initial observation cannot distinguish an early I/O stop from a deliberately
sent TTIN/TTOU; a late startup stop also uses suspension. There is no lifetime
allowance, unconditional continuation, second waiter or startup timeout.

A deliberate foreground stop restores the saved terminal and parks input/output
before requesting app-owned suspension. The app owns signal policy and the
capacity-one request receiver. It grants resume only after fresh continuation
and restored foreground ownership. The terminal rechecks ownership before
reattachment; the supervisor retains child signals and reap. The app closes and
rejects queued or blocked suspension requests before joining workers.

Parent modes and the most recent stopped-child modes are separate snapshots.
The terminal saves child modes before restoring the parent. After a valid resume
grant it verifies ownership, reattaches the child group and restores child modes
before the supervisor sends SIGCONT. Failures attempt parent restoration and
retain the first source followed by cleanup errors in their actual order.

The supported Linux self-stop contract assumes an ordinary stable parent shell
in the same session and outside confctl's process group. An owned direct-parent
pidfd witnesses parent liveness. Unsupported parent relationships, dispositions
or pidfd support fail explicitly; no universal guarantee against hostile parent
process-group changes is claimed. The app uses safe SIGTSTP raise and fresh
SIGCONT/cancellation observation, checking parent liveness and foreground
readiness while parked.

## Verification boundary

Buffer tests check stable quiet/busy geometry, aligned completion counts and
percentages, long log paths and compact checkpoint projections.
Painter tests check that identical frames write nothing and a changed row does
not rewrite the static header. Controlling-PTY fixtures exercise burst activity,
quiet command starts, resize, prompt and foreground handoff, stop/continue and
Ctrl-C, terminal restoration, final-panel/footer order and capture retention after
output faults. These traces check routing and redraw behavior; they do not prove
visual comfort in every terminal. A PTY without a controlling session cannot
establish job-control correctness. Real-site builds and host operations require
their own qualification.

Checked application shutdown joins operation/input/process/presentation owners
before the single final stderr timing appendix. Presentation does not own timing
admission or print a separate footer. The model and plain formatting tests do not
replace these native integration gates.
