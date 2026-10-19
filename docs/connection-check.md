# Check machine connections

`confctl test-connection [pattern]` reads `/proc/uptime` on each selected
machine. It selects managed machines by default and excludes carried machines
and machines with a null target. Patterns, machine keys, attribute filters and
tag filters work as for other machine commands. Use `--managed=all` to include
unmanaged machines, or `--managed=no` to select only unmanaged machines.

The command shows the machine table and asks for confirmation. `--yes` skips
the preview and prompt. An empty selection or declined confirmation exits 1
before probing any host.

Probes run serially. A `localhost` target runs `cat /proc/uptime` locally;
other targets use SSH as root, the configured port and a nonempty
`CONFCTL_SSH_CONFIG` when supplied. SSH uses `ConnectTimeout=3`,
`ServerAliveInterval=3` and `ServerAliveCountMax=1`. Successful uptime output
stays hidden. The result reports successful and failed counts and lists failed
machines. Ordinary SSH or local command failures do not stop later probes.
As in the Ruby command, the completed command exits 0 even if every host fails.

Process startup, capture and presentation failures stop the invocation.
Cancellation exits 130 after joining the active child; later hosts do not run.
The terminal view distinguishes pending, entered and finished host probes and
shows elapsed time without estimating progress.

The probes do not change remote configuration, generations, GC roots or flake
locks, and do not acquire the configuration mutation lock. Normal invocation
logging still applies. The Rust development entrypoint is `confctl-rust`.
