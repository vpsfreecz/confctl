# Add, rename and rediscover machines

`confctl add NAME`, `rename OLD NEW` and `rediscover` require a flake
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

In a Git-backed flake, Nix includes tracked files but excludes newly untracked
files, even with impure evaluation. With netboot publication enabled, `add` or
`rename` can therefore complete its local edits and then fail because the fresh
inventory cannot import a new or moved `module.nix`. The old netboot list remains.
Inspect the completed edits, explicitly stage only the intended configuration
paths, then run `confctl rediscover`. For example:

```sh
git add -- cluster/NEW cluster/cluster.nix
confctl rediscover
```

For a rename, stage both the old and new paths with
`git add -A -- cluster/OLD cluster/NEW cluster/cluster.nix` before rediscovering.
This is the same source-inclusion prerequisite as the former Ruby hook.
Confctl never stages these files automatically. Do not repeat `add` or `rename`
after their filesystem changes have completed.

The generated formats remain readable by the preceding Ruby implementation.
The public executable selects Rust. Enabling netboot publication requires the module/tool pin defining
`netbootInventory`, explicit enable/outputFile settings and removal of the old
hook script. Restore the matched old pin, settings and script together when
rolling back that migration: reverting only the tool after removing the script
would omit refresh. Preserve generated lists and local state.

The ignored `configuration_real_nix` test is a synthetic source-inclusion and
site-import qualification, separate from the default fake-tool tests. In an
already available environment, set absolute `CONFCTL_REAL_NIX_BIN` (the
designated integrated binary), `CONFCTL_REAL_NIX_SOURCE` (held stable),
`CONFCTL_REAL_NIX_NIXPKGS`, `CONFCTL_REAL_NIX_SITE_HELPER` (the actual site
`lib/netboot-machines.nix`), `CONFCTL_REAL_NIX_NIX`, `CONFCTL_REAL_NIX_GIT` and
`CONFCTL_REAL_NIX_SHELL`, plus the native `CONFCTL_REAL_NIX_SYSTEM` and a new
`CONFCTL_REAL_NIX_ROOT` outside every Git ancestor. All sources, tools and
writeText builder dependencies must already be available; missing prerequisites
fail explicitly. After authorization, select `CONFCTL_REAL_NIX_PHASE=baseline`
for `cargo test --offline --locked --test configuration_real_nix -- --ignored
--nocapture`. This prepares only local-input fixtures and stops after one offline
machinesJson dry-run. Inspect its output: only the small writeText result may
need building. A separate authorization with `CONFCTL_REAL_NIX_PHASE=cases`
and the same command/root runs the five cases and explicit fixture-index
controls. Nix uses the disclosed offline/no-substitute/no-remote-builder prefix
and empty registry; observers are separate from product calls. Fixtures and raw
outputs remain in that root for disposition. Neither an ignored test nor a
successful baseline dry-run closes the qualification gate. These cases do not
qualify full site or system builds.
