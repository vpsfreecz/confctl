# Add, rename and rediscover machines

`confctl-rust add NAME`, `rename OLD NEW` and `rediscover` require a flake
configuration. They run without confirmation. Each command holds the existing
advisory mutation lock through its local edits, Nix evaluation and netboot
publication. This excludes cooperating Rust writers; Ruby and external editors
do not participate in that lock.

Add creates `cluster/NAME` and its missing parents, then writes the standard
`module.nix` and `config.nix`. Nested names determine the base import depth and
hostname exactly as in Ruby. New directories use 0755 and template files use
0644, restricted by umask. Rename creates missing destination parents and moves
the source directory once, preserving its contents. It does not rewrite embedded
cluster names, hostnames, imports, generations, current links or kernel keys.
Existing destinations, including dangling links, cause a failure.

Names retain their native path bytes. Empty or absolute names and empty, `.` or
`..` components are refused. Resolved parents must stay within `cluster`.
Rediscovery follows internal directory links but refuses escaping links and
ancestor cycles. These path refusals intentionally narrow Ruby's behavior;
the command does not normalize names or repair their Nix syntax.

Rediscovery visits hidden and nested directories, including descendants of an
already qualifying directory. A directory qualifies when both `module.nix` and
`config.nix` exist. It sorts their relative names and replaces
`cluster/cluster.nix` through a fresh sibling file. The replacement uses 0644
subject to umask, rather than preserving the previous core-list mode.
Add and rename finish with the same rediscovery step.

Before editing, these commands refuse any direct regular file in `scripts`,
including a symlink resolving to a file and files without a Ruby extension.
Migrate the hook to the matching module and settings, then remove the legacy
script. The command never executes or removes it. Subdirectories alone do not
trigger this migration refusal.

After replacing the core list, the command reads settings for the first time.
Absent netboot settings in a valid settings object mean disabled: this costs
one pure stateless settings query and leaves an existing generated netboot list
untouched. Missing, invalid or failed settings fail the command even when core
edits succeeded. There is no disabled-on-error fallback.

When `netbootInventory.enable` is true, a fresh configured Nix client separately
reads settings using the captured job count, then builds fresh `machinesJson`.
The resulting inventory produces `netbootInventory.outputFile` through the
existing publisher. The list retains inventory order and includes non-carried
machines whose `netboot.enable` is true, regardless of spin, managed status or
runnable target. The normal enabled sequence is two settings queries followed
by one inventory build, all after the core list replacement. It does not build
systems, contact hosts or refresh runtime kernels.

Notices are acknowledged before their effects. A write, output, Nix or
cancellation failure retains completed files, directory moves and core lists;
a failed staged write can also leave its fresh sibling. A failure before feature
publication leaves the previous netboot list. Inspect the reported stage and
correct the configuration, then explicitly run `rediscover`. Blindly repeating
add or rename can conflict with effects that already completed. There is no
rollback, automatic repair or resume.

The generated formats remain readable by Ruby and the default executable remains
Ruby. Enabling netboot publication requires the module/tool pin defining
`netbootInventory`, explicit enable/outputFile settings and removal of the old
hook script. Restore the matched old pin, settings and script together when
rolling back that migration: reverting only the tool after removing the script
would omit refresh. Preserve generated lists and local state.
