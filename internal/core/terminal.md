# Linux terminal bindings

`SyscallConn` exposes a borrowed descriptor without changing its blocking mode.
`x/term` detects terminals and reads dimensions; `x/sys/unix` supplies termios
structures and ioctl bindings. Confctl owns descriptors, mode policy and cleanup.

SSH retains its controlling-terminal selection and per-dimension
24-row/80-column fallback. Library dimensions arrive as columns, rows and are
mapped explicitly into the caller's row/column order.

Inherited SSH and Git job control keep their distinct process and child-wait
owners. Live command logs retain their operation and machine context.
