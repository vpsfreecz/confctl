# Run commands and open shells

`confctl ssh [options] pattern command [arguments...]` runs the requested command
on selected machines. Managed machines are selected by default. Use
`--managed=all` to include unmanaged machines or `--managed=no` to select only
unmanaged machines. Machine keys, patterns, attributes and tags use the usual
selection rules. Carried machines and machines with null targets are excluded.
An empty selection fails before any command runs.

The confirmation shows the machine table and command. `--yes` skips the preview
and prompt. A declined confirmation does not start children. Commands run
serially unless `--parallel` is supplied. Each host receives its own input from
`--input-string` or `--input-file`; file paths are relative to the configuration
directory. These two input options are mutually exclusive.

A `localhost` target executes the supplied argv directly. Other targets use root
SSH with the configured port and a nonempty `CONFCTL_SSH_CONFIG` when provided.
Remote arguments form one Shellwords-quoted command string. Empty arguments,
quotes, newlines, metacharacters and native argument bytes remain literal.

Stdout and stderr share one native pipe. Each host result retains their combined
bytes in write order, including output from failed commands. `--aggregate`
groups only equal native results with identical combined output, and sorts host
names within each group. Exit codes and signals are distinct results. Ordinary
nonzero or signaled children do not stop the remaining hosts; a completed command
invocation exits 0. Process, input, capture or presentation errors fail the
invocation. Cancellation joins entered work, leaves later work unentered and
exits 130; already observed host outcomes remain visible.

Without a command, `confctl ssh pattern` requires exactly one selected machine.
It opens plain SSH for a remote target, or runs UID 0's passwd shell locally
without changing the current user. The local shell does not come from `$SHELL`.
Input-file, input-string, parallel and aggregate options do not change this
interactive branch. The terminal owner parks live painting during the shell and
restores input ownership and terminal settings after it exits. Ordinary shell
exit failures remain host results; terminal and process startup errors fail the
invocation.

The live command view reports entered work and actual completed and failed host
counts. SSH leaves confctl generations and GC roots unchanged; commands can have
the effects requested by their caller. The Rust development entrypoint is
`confctl-rust`.
