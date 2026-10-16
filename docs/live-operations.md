# Live operation views

When both standard output and standard error are terminals, confctl shows the
current operation, task states, progress and recent output in one live view.
The compact header identifies the confctl command, operation or build group,
target and action where applicable, and the full command log when enabled.
That file is available during the operation. Afterwards,
failed or explicitly retained logs remain available under `.confctl/logs`;
successful commands normally remove their logs. Redirecting either output stream
or setting `CONFCTL_TTY=0` selects ordinary text output. Color settings remain
independent of this choice.

| Operation | What the view reports |
| --- | --- |
| Build, build-backed status and deployment builds | Build group and machine count, Building and Fetching progress, combined nix build counts when reported, and the current nix eval. Completed build work keeps its outcome during later evaluation. |
| Sequential copy | Target and copied paths after permission is granted, including empty work and failures. |
| Concurrent copy | One shared queue view with host and aggregate path counts. |
| Activation | Machine and action, dry activation, profile switch and rollback-helper output. |
| Health checks | One round's actual checks, attempts and final task states. A failed attempt does not complete a check. |
| Parallel SSH commands | Live command output, completed-command counts and per-host status. Final results keep their normal completion/aggregation order. |
| Generation garbage collection | Deleted paths by host and in aggregate, followed by the existing final statistics. |
| Reboot waiting | Reboot, waiting down/up and observed-up phases, with elapsed time and remaining time when limited. |

Positive known totals show a percentage and, when space permits, a progress bar.
Unknown totals show activity and a count without an estimated percentage. A zero
total has no percentage and does not imply a successful operation. Build and
fetch counts describe observed Nix activity; their final state follows the
build process result. Evaluation appears in its own row.
Pending, running, succeeded, failed, skipped and incomplete tasks have
distinct states. Resizing reflows the live view. When tasks exceed the available
rows, an overflow line counts hidden tasks, including failed and incomplete
tasks. When the view closes, its completed panel stays visible, followed by
only the task rows omitted from that panel, with their full labels and counts.
A clipped or missing full-log locator is printed in full after those rows.
The command then prints its ordinary results.

Visible task rows align their counts, bars and details. On narrow terminals,
bars shrink or disappear, and long labels or counts are visibly clipped.
Store paths in task details keep
their full hash when they fit; otherwise the display shortens a canonical
store hash to its first seven characters and an ellipsis. Logs and command
results keep their original paths.

Before each build group, confctl prints its complete machine and input list once
as ordinary scrolling output. The list stays in terminal scrollback while the
compact group header remains visible. The bordered Live view shows recent plain
log output above progress bars and host rows. At 80 columns by 24 rows it reserves
at least twelve log lines and at most six progress rows, including any overflow
count. Smaller terminals reduce the display to the space available.

Confctl headings are bold; machine names and actions are yellow, generation
names cyan, success green, failure red, and skipped or incomplete states yellow.
`--color=always` enables these styles even when `NO_COLOR` is set;
`--color=never` disables them. Auto color requires the actual output destination
to be a terminal and `NO_COLOR` to be empty or unset. Child output stays plain,
including when color is forced on.

The controls are the command's existing confirmations and retry choices. Painting
stops while a prompt owns the terminal, including dry-activation choices and
health retry/continue choices. Queued answers use the same input reader. Signals
retain their existing cancellation behavior; a view never reads keys or changes
terminal input mode. Interactive SSH and comparison paging retain their separate
terminal behavior, including the Basic pager's q/Q controls.

Build, activation and parallel SSH display a bounded tail of the checked command
log in file order. Other views display their operation's progress messages.
Recent display history is bounded, and child control sequences are normalized
for safe display; the full log and command captures keep their original bytes.
Final output is drained after children finish, including cancellation. Inspect
the retained log and per-host results after an interrupted operation before
deciding whether to retry: already completed effects remain completed.
