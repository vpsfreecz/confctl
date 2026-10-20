# Open ClusterSSH

`confctl cssh [options] pattern` opens ClusterSSH for the selected machines.
Managed machines are selected by default. `--managed=all` includes unmanaged
machines; `--managed=no` selects only unmanaged machines. Patterns, machine keys,
attributes and tags follow the usual selection rules. Carried machines and
machines with null targets are excluded. An empty selection fails.

Confirm the machine table before launching, or pass `--yes` to skip the preview
and prompt. Declining starts no launcher. The command runs one
`nix-shell -p perlPackages.AppClusterSSH --command ...` with `cssh -l root`
and the selected target names in inventory order. Targets are shell-quoted as
literal arguments. A `localhost` target is passed to ClusterSSH too. Per-host
ports and `CONFCTL_SSH_CONFIG` do not change this command.

The launcher inherits the display, authentication and other environment
variables, with `shellHook` removed. It may evaluate or download Nix packages and
open graphical terminals and remote sessions. Stdout and stderr are forwarded
and retained in the invocation log. The task reports the launcher's result;
a nonzero exit or signal produces a failure diagnostic while the completed
invocation exits 0, matching the Ruby command. Startup, capture or output errors
fail the invocation. Cancellation joins the owned launcher and exits 130.

A successful launcher exit does not prove that every host connected. Opened
windows or remote shells may outlive the launcher; confctl does not manage
their lifetime. If launching fails, correct the display, package or connectivity
problem and rerun explicitly. Confctl leaves saved generations, profiles and
configuration locks unchanged. The Rust development entrypoint is `confctl-rust`.
