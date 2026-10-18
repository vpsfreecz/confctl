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
