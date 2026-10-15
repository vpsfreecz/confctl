# Linux terminal bindings

`SyscallConn` exposes a borrowed descriptor without changing its blocking mode.
`x/term` detects terminals; confctl owns the descriptor and its lifetime.

Git job control retains its process and child-wait owners. Live command logs
retain their operation and machine context.
