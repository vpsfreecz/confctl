# Linux terminal bindings

The Basic pager borrows its original input descriptor and one retained
`bufio.Reader`. `SyscallConn` exposes the descriptor without changing its blocking
mode. `x/term` detects terminals and reads dimensions; `x/sys/unix` owns termios
and poll structures and ioctl bindings. These libraries supply primitives, while
confctl owns input, mode policy and cleanup.

Basic size discovery checks stdout, eligible stderr, then its existing
environment/default fallback. SSH retains its controlling-terminal selection
and per-dimension 24-row/80-column fallback. Library dimensions arrive as columns,
rows and are mapped explicitly into the caller's row/column order.

Basic's key interval has its existing raw-mode policy. A reached set attempt
retains the existing restoration/error precedence, including cancellation or
failed application. Descriptor replacement is refused before restoration.

The shared `inputWait` consumes buffered bytes before polling, retries EINTR,
and owns only its wake pipe and notifier. Cancellation wakes polling without
closing borrowed stdin. The caller joins the notifier before restoring the
terminal and closing acquired ends; no delayed read or notifier survives return.
`Ppoll` handles input, wake readiness, HUP and errors, with POLLNVAL reported as
EBADF.

Inherited SSH, Git job control and SystemPager keep their distinct process and
child-wait owners. Live command logs retain their operation and machine context;
this terminal-library change does not replace them with a deferred display.
See [comparison paging](../ui/README.md) for wrapping and pager boundaries.
