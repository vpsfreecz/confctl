# Linux terminal bindings

The Basic pager and authentication prompt borrow their original input descriptor
and one retained `bufio.Reader`. `SyscallConn` exposes the descriptor without
changing its blocking mode. `x/term` detects terminals and reads dimensions;
`x/sys/unix` owns termios and poll structures and ioctl bindings. These libraries
supply primitives, while confctl owns input, mode policy and cleanup.

Basic size discovery checks stdout, eligible stderr, then its existing
environment/default fallback. SSH retains its controlling-terminal selection
and per-dimension 24-row/80-column fallback. Library dimensions arrive as columns,
rows and are mapped explicitly into the caller's row/column order.

Basic and authentication keep separate small termios masks. Authentication keeps
ISIG and OPOST; Basic's key interval has its existing raw-mode policy. Neither
uses `MakeRaw` or `ReadPassword`. A reached set attempt retains the existing
restoration/error precedence, including cancellation or failed application.
Descriptor replacement is refused before restoration.

The shared `inputWait` consumes buffered bytes before polling, retries EINTR,
and owns only its wake pipe and notifier. Cancellation wakes polling without
closing borrowed stdin. The caller joins the notifier before restoring the
terminal and closing acquired ends; no delayed read or notifier survives return.
`Ppoll` handles input, wake readiness, HUP and errors, with POLLNVAL reported as
EBADF. Prompt permission and serialized extension confirmations remain with their
existing owners.

Inherited SSH, Git job control and SystemPager keep their distinct process and
child-wait owners. The shared live session owns one output region, resize
notification and a bounded display model. It never reads stdin or changes
termios. Its prompt barrier stops painting before the existing prompt runs and
resumes only after successful completion. Rendering errors latch once and notify
the operation outside model/output locks; shutdown joins that notification.

Before its first home repaint the session scrolls one viewport of ordinary LF,
preserving the caller's once-only machine/input prelude in terminal history.
Actual geometry changes reserve a new viewport once. A resize during a prompt
records dimensions but defers scrolling and painting until successful resume or
final teardown after the prompt returns. Unchanged resize notifications and
timer frames do not reserve again. Every complete paint positions the cursor in
the last reserved row; a one-row terminal positions without erasing its only row.
Color policy resolves against the supplied destination, independently of live
TTY admission. Sanitized data receives only fixed confctl field/state SGR with
balanced resets; raw and semantic child records remain plain.

Raw live views reopen the checked command log read-only, validate file identity
and start at the existing end. Bounded reads follow file offsets; synchronous
log subscribers are not an ordered payload source. Flush captures a finite end,
and close always drains the final stable end after child/callback joins, even
when the operation context was cancelled. Only acquired read/console descriptors
are closed. The renderer owns no child, input reader or execution policy.
See [live operations](../../docs/live-operations.md) for progress meaning,
operator controls, text fallback and full-log access.
See [comparison paging](../ui/README.md) for wrapping and pager boundaries.
