# Initialize a configuration

Run `confctl-rust init` in an empty directory. The command creates the standard
flake configuration without confirmation or external commands. It does not need
an existing flake, evaluate Nix, initialize Git, generate a lock file or build a
system.

The directory may already contain `shell.nix`, `.confctl`, `.gems` and
`.gitignore`; init preserves them. Every other entry prevents initialization,
including `.git`, `flake.nix` and `cluster`. A second init therefore fails.

Init creates `cluster`, `configs`, `data`, `environments` and `modules`, their
starter Nix files, and finally `flake.nix`. The templates match Ruby confctl,
including the `nixos-unstable` channel and commented vpsAdminOS input. New
directories use mode 0755 and files use 0644, restricted by the process umask.
The normal invocation log and advisory mutation lock live in `.confctl`.
Ruby writers do not participate in that lock.

Each `mkdir` or `mkfile` notice is delivered before its effect. Init stops on
the first conflict, write failure, output failure or cancellation and retains
completed files and any partially written file. It never overwrites a new
conflicting destination or rolls back the partial tree. Inspect or move that
tree, then explicitly initialize an empty directory. Init does not rediscover
machines or run the rediscovery hook.
