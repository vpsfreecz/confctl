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

The live model keeps task registration order, explicit final results, bounded
sanitized tail lines and full log locators. Visible membership prioritizes failed,
incomplete and canceled work before restoring registration order. Unknown or zero
totals show counts; positive totals permit a clamped progress bar. Narrow rows
keep the result before a clipped description. Rendering reads elapsed time
without freezing the timing ledger.

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

`status` with an explicit generation admits one task per confirmed, resolved
host observation. Saved status compares the saved toplevel and input metadata
with deployed state; `--generation none` evaluates current target inputs instead.
Both modes read deployed uptime, profiles and inputs; saved status also reads
the running system or carried profile path. Missing saved targets admit no task.
Failed host observations fail their tasks and leave offline or unknown table
cells; ordinary host failures still allow the completed command to exit 0.
Cancellation and delivery failures join entered observations. Status with an
omitted generation remains unavailable.

`ssh` admits per-host tasks and reports actual completed and failed host counts
for commands. Command stdout and stderr share one native pipe and capture.
Interactive shells park painting through the foreground terminal owner and
restore terminal and input ownership on return. Ordinary nonzero child results
fail their tasks while allowing the completed command to exit 0.

`build` registers one task per actual build group after consent and planning.
Groups run serially; a shared Nix build does not prove that each host executed
separately. Stderr streams through the existing checked output relay while stdout
stays captured for JSON decoding. Bounded incremental parsing recognizes Nix's
build/fetch summaries and activity lines; unknown output remains visible and raw
captures are unchanged. Counts describe activity, with an indeterminate total
until both totals are known, and reset for each native attempt. A successful
group includes generation reuse/publication. On failure, later groups are skipped
or canceled and earlier saved generations and partial roots remain in place.
Task log locators refer to the app-owned invocation log. Successful finalization
removes that log unless the operation explicitly requests retention; failures
retain it under the existing policy.

Saved-generation copy-only deploys admit one task per resolved host, with bounded
copying-count/path progress from native stderr and unknown totals when absent.
Optional runtime-kernel refresh uses distinct preceding task IDs. Copy results do
not claim activation. Saved-generation boot and dry-activate add separate bounded
status-query tasks, copy tasks and standalone activation tasks. Activation covers
a preliminary dry run when requested and the requested action; only boot sets
the profile. Carried hosts have only the copy effect. Delivered task results
remain actual observations if later output or cancellation fails the invocation.
Default/new-build deploy, switch, test and reboot remain unavailable; other
operation producers are not wired to this lifecycle yet. Controlled route
fixtures do not establish real-site activation or full live-terminal qualification.

`health-check` admits one task per configured logical check and starts it on
entry to the bounded check runner. Attempts share that task and its log locator;
failed attempts remain visible even when a retry succeeds. A failed assessment
requests retention of the raw invocation log before diagnostics or cooldown.
The final summary reports actual passed and failed checks. Cancellation and
delivery failures join entered work and preserve already delivered results.

Live tail fragments and task rows share a compact source key: @ followed by
the complete TaskId in base 36. Tail suffixes /o and /e identify stdout and
stderr. A source change starts a separate displayed fragment, so partial lines
from different sources do not merge. Keys encode the supplied ID directly;
host and stage descriptions still come from the operation.

Narrow views keep complete keys when they can fit beside useful message text.
An ellipsis marks shortened text or omitted fields. Tiny views omit a whole
key rather than showing an ambiguous prefix. Final summaries retain the key,
full decimal TaskId, description, outcome and counts for tasks omitted or
shortened by the final layout. Retained output without a registered task gets
a source association with an explicit missing description, without inventing a
task state. Tail content and its source metadata remain bounded.

Each retained task/log locator is also summarized at group finalization, even
if its header was visible. Paths use one quoted, single-line display encoding:
backslashes and quotes are escaped; LF, CR and tab use visible escapes; other
ASCII controls and non-UTF8 bytes use two-digit byte escapes. Unicode controls,
line separators and directional controls are escaped, while printable Unicode,
including wide and combining text, remains readable. A header locator that
cannot fit is replaced by an ellipsis; the final summary retains it in full.
Native PathBuf values remain unchanged. Display labels affect only the view.
Returned sanitized child output and original captures retain their existing
bytes and stream roles.

Both output streams must be usable terminals for live mode. `CONFCTL_TTY=0` or a
redirected stream selects plain mode. Color policy is separate: `always` forces
confctl styling, `never` disables it, and `auto` follows the destination and
`NO_COLOR`. Child data and captured paths are not recolored or changed.

## Why the painter uses relative output commands

Ratatui widgets render into a `Buffer`. The writer paints it with output-only
Crossterm move/clear commands in the normal screen. It establishes a known line
anchor through its own output, reserves a terminal column to avoid soft wrapping,
and invalidates its geometry after resize or terminal handoff. It never asks the
terminal for cursor coordinates, enters an alternate screen, changes raw mode,
or reads keys. Resize comes from SIGWINCH and terminal-size ioctl.

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

The renderer and controlling-TTY integration are additive development work.
Native attachment primitives, real Supervisor worker fixtures, app self-stop
and the full app/worker path have passed focused checks. The notification-poll
ownership correction described above also passed owned-zombie, external-reap
and initial-attachment exit regressions. Interactive operation routes and the
default package remain unreleased.
A PTY without a controlling session cannot establish job-control correctness.
Required tests cover input typeahead, no cursor query/raw mutation, narrow screens
and resize, prompt and foreground
handoff, stop/continue and Ctrl-C, parent exit around suspension, final panel and
footer order, and capture/cleanup retention after output faults.

Checked application shutdown joins operation/input/process/presentation owners
before the single final stderr timing appendix. Presentation does not own timing
admission or print a separate footer. The model and plain formatting tests do not
replace these native integration gates.
