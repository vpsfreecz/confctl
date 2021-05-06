# confctl 8                       2020-11-01                             master

## NAME
`confctl` - Nix deployment management tool

## SYNOPSIS
`confctl` [*global options*] *command* [*command options*] [*arguments...*]

## DESCRIPTION
`confctl` is a Nix deployment configuration management tool. It can be used to
build and deploy *NixOS* and *vpsAdminOS* machines.

## SOFTWARE PINS
Each machine managed by `confctl` uses predefined software packages
like `nixpkgs`, `vpsadminos` and possibly other components. These packages
are pinned to particular versions, e.g. specific git commits.

Software pins are defined in the Nix configuration and then prefetched using
`confctl`. Selected software pins are added to environment variable `NIX_PATH`
for `nix-build` and can also be read by `Nix` while building machines.

Software pins can be defined using channels or on specific machines.
The advantage of using channels is that changing a pin in a channel changes
also all machines that use the channel. Channels are defined in file
`configs/swpins.nix` using option `confctl.swpins.channels`. Deployments
declare software pins and channels in their respective `module.nix` files,
option `cluster.<name>.swpins.channels` is a list of channels to use and option
`cluster.<name>.swpins.pins` is an attrset of pins to extend or override pins
from channels.

Software pins declared in the Nix configuration have to be prefetched before
they can be used to build machines. See the `confctl swpins` command family
for more information.

## PATTERNS
`confctl` commands accept patterns instead of names. These patterns work
similarly to shell patterns, see
<http://ruby-doc.org/core/File.html#method-c-fnmatch-3F> for more
information.

## GLOBAL OPTIONS
`-c`, `--color` `always`|`never`|`auto`
  Set output color mode. Defaults to `auto`, which enables colors when
  the standard output is connected to a terminal.

## COMMANDS
`confctl init`
  Create a new configuration in the current directory. The current directory
  is set up to be used with `confctl`.

`confctl add` *name*
  Add new machine to the configuration.

`confctl rename` *old-name* *new-name*
  Rename machine from the configuration.

`confctl rediscover`
  Auto-discover machines within the `cluster/` directory and generate a list
  of their modules in `cluster/cluster.nix`.

`confctl ls` [*options*] [*machine-pattern*]
  List matching machines available for deployment.

    `--show-trace`
      Enable traces in Nix.

    `--managed` `y`|`yes`|`n`|`no`|`a`|`all`
      The configuration can contain machines which are not managed by confctl
      and are there just for reference. This option determines what kind of
      machines should be listed.

    `-L`, `--list`
      List possible attributes that can be used with options `--output`
      or `--attr` and exit.

    `-o`, `--output` *attributes*
      Comma-separated list of attributes to output. Defaults to the value
      of option `confctl.list.columns`.

    `-H`, `--hide-header`
      Do not print the line with column labels.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter machines by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter machines that have *tag* set. If the tag begins with `^`, then
      filter machines that do not have *tag* set.

`confctl build` [*options*] [*machine-pattern*]
  Build matching machines. The result of a build is one generation for each
  built machine. Subsequent builds either return an existing generation if there
  had been no changes for a machine or a new generation is created. Built
  generations can be managed using `confctl generation` command family.

    `--show-trace`
      Enable traces in Nix.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter machines by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter machines that have *tag* set. If the tag begins with `^`, then
      filter machines that do not have *tag* set.

    `-y`, `--yes`
      Do not ask for confirmation on standard input, assume the answer is yes.

    `-j`, `--max-jobs` *number*
      Maximum number of build jobs, passed to `nix-build`. See man nix-build(1).

`confctl deploy` [*options*] [*machine-pattern* [`boot`|`switch`|`test`|`dry-activate`]]
  Deploy either a new or an existing build generation to matching machines.

  *switch-action* is the argument to `switch-to-configuration` called on
  the target machine. The default action is `switch`.

    `--show-trace`
      Enable traces in Nix.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter machines by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter machines that have *tag* set. If the tag begins with `^`, then
      filter machines that do not have *tag* set.

    `-y`, `--yes`
      Do not ask for confirmation on standard input, assume the answer is yes.

    `-g`, `--generation` *generation*|`current`
      Do not build a new generation, but deploy an existing generation.

    `-i`, `--interactive`
      Deploy machines one by one while asking for confirmation for activation.

    `--dry-activate-first`
      After the new system is copied to the target machine, try to switch the
      configuration using *dry-activate* action to see what would happen before
      the real switch.

    `--one-by-one`
      Instead of copying the systems to all machines in bulk before activations,
      copy and deploy machines one by one.

    `--max-concurrent-copy` *n*
      Use at most *n* concurrent nix-copy-closure processes to deploy closures
      to the target machines. Defaults to `5`.

    `--copy-only`
      Do not activate the copied closures.

    `--reboot`
      Applicable only when *switch-action* is `boot`. Reboot the machine after the
      configuration is activated.

    `--wait-online` [*seconds* | `wait` | `nowait`]
      Determines whether to wait for the machines to come back online
      if `--reboot` is used. `confctl` will wait for `600 seconds` by default.

    `-j`, `--max-jobs` *number*
      Maximum number of build jobs, passed to `nix-build`. See man nix-build(1).

`confctl status` [*options*] [*machine-pattern*]
  Probe managed machines and determine their status.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter machines by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter machines that have *tag* set. If the tag begins with `^`, then
      filter machines that do not have *tag* set.

    `-y`, `--yes`
      Do not ask for confirmation on standard input, assume the answer is yes.

    `-g`, `--generation` *generation*|`current`|`none`
      Check status against a selected generation instead of a new build. If set
      to `none`, only the currently configured software pins are checked and not
      the system version itself.

    `-j`, `--max-jobs` *number*
      Maximum number of build jobs, passed to `nix-build`. See man nix-build(1).

`confctl changelog` [*options*] [*machine-pattern* [*sw-pattern*]]
  Show differences in deployed and configured software pins. For git software
  pins, it's a git log.

  By default, `confctl` assumes that the configuration contains upgraded
  software pins, i.e. that the configuration is equal to or ahead of the deployed
  machines. `confctl changelog` then prints a lists of changes that are missing
  from the deployed machines. Too see a changelog for downgrade, use option
  `-d`, `--downgrade`.

  `confctl changelog` will not show changes to the deployment configuration
  itself, it works only on software pins.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter machines by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter machines that have *tag* set. If the tag begins with `^`, then
      filter machines that do not have *tag* set.

    `-y`, `--yes`
      Do not ask for confirmation on standard input, assume the answer is yes.

    `-g`, `--generation` *generation*|`current`
      Show changelog against software pins from a selected generation instead
      of the current configuration.

    `-d`, `--downgrade`
      Use when the configuration has older software pins than deployed machines,
      e.g. when doing a downgrade. Show a list of changes that are deployed
      on the machines and are missing in the configured software pins.

    `-v`, `--verbose`
      Show full-length changelog descriptions.

    `-p`, `--patch`
      Show patches.

    `-j`, `--max-jobs` *number*
      Maximum number of build jobs, passed to `nix-build`. See man nix-build(1).

`confctl diff` [*options*] [*machine-pattern* [*sw-pattern*]]
  Show differences in deployed and configured software pins. For git software
  pins, it's a git diff.

  By default, `confctl` assumes that the configuration contains upgraded
  software pins, i.e. that the configuration is equal to or ahead of the deployed
  machines. `confctl diff` then considers changes that are missing from the
  deployed machines. Too see a diff for downgrade, use option
  `-d`, `--downgrade`.

  `confctl diff` will not show changes to the deployment configuration
  itself, it works only on software pins.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter machines by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter machines that have *tag* set. If the tag begins with `^`, then
      filter machines that do not have *tag* set.

    `-y`, `--yes`
      Do not ask for confirmation on standard input, assume the answer is yes.

    `-g`, `--generation` *generation*|`current`
      Show diff against software pins from a selected generation instead
      of the current configuration.

    `-d`, `--downgrade`
      Use when the configuration has older software pins than deployed machines,
      e.g. when doing a downgrade. Show a list of changes that are deployed
      on the machines and are missing in the configured software pins.

    `-j`, `--max-jobs` *number*
      Maximum number of build jobs, passed to `nix-build`. See man nix-build(1).

`confctl cssh` [*options*] [*machine-pattern*]
  Open ClusterSSH to selected or all machines.

    `--managed` `y`|`yes`|`n`|`no`|`a`|`all`
      The configuration can contain machines which are not managed by confctl
      and are there just for reference. This option determines what kind of
      machines should be selected.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter machines by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter machines that have *tag* set. If the tag begins with `^`, then
      filter machines that do not have *tag* set.

    `-y`, `--yes`
      Do not ask for confirmation on standard input, assume the answer is yes.

`confctl generation ls` [*machine-pattern* [*generation-pattern*]]
  List all or selected generations. By default only local build generations
  are listed.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter machines by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter machines that have *tag* set. If the tag begins with `^`, then
      filter machines that do not have *tag* set.

    `-l`, `--local`
      List build generations.

    `-r`, `--remote`
      List remote generations found on deployed machines.

`confctl generation rm` [*machine-pattern* [*generation-pattern*|`old`]]
  Remove selected generations.

  `old` will remove all generations except the current one, i.e. the one that
  was built by `confctl build` the last.

  By default, only local build generations are considered.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter machines by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter machines that have *tag* set. If the tag begins with `^`, then
      filter machines that do not have *tag* set.

    `-l`, `--local`
      Consider local build generations.

    `-r`, `--remote`
      Consider generations found on deployed machines.

    `-y`, `--yes`
      Do not ask for confirmation on standard input, assume the answer is yes.

`confctl generation rotate` [*options*] [*machine-pattern*]
  Delete old build generations of all or selected machines. Old generations are
  deleted based on rules configured in `configs/confctl.nix`.

  This command deletes old build generations from `confctl`. To remove them
  from the Nix store, use `nix-collect-garbage` afterwards.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter machines by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter machines that have *tag* set. If the tag begins with `^`, then
      filter machines that do not have *tag* set.

    `-y`, `--yes`
      Do not ask for confirmation on standard input, assume the answer is yes.

    `-l`, `--local`
      Consider local build generations.

    `-r`, `--remote`
      Consider generations found on deployed machines.

`confctl gen-data vpsadmin all`
  Generate all required data files from vpsAdmin API.

`confctl gen-data vpsadmin containers`
  Generate container data files from vpsAdmin API.

`confctl gen-data vpsadmin network`
  Generate network data files from vpsAdmin API.

`confctl swpins cluster ls` [*name-pattern* [*sw-pattern*]]
  List cluster machines with pinned software packages.

`confctl swpins cluster set` *name-pattern* *sw-pattern* *version...*
  Set selected software packages to new *version*. The value of *version* depends
  on the type of the software pin, for git it is a git reference, e.g. a revision.

`confctl swpins cluster update` [*name-pattern* [*sw-pattern*]]
  Update selected or all software packages that have been configured to support
  this command. The usual case for git is to pin to the current branch head.

`confctl swpins channel ls` [*channel-pattern* [*sw-pattern*]]
  List existing channels with pinned software packages.

`confctl swpins channel set` *channel-pattern* *sw-pattern* *version...*
  Set selected software packages in channels to new *version*. The value
  of *version* depends on the type of the software pin, for git it is a git
  reference, e.g. a revision.

`confctl swpins channel update` [*channel-pattern* [*sw-pattern*]]
  Update selected or all software packages in channels that have been configured
  to support this command. The usual case for git is to pin to the current
  branch head.

`confctl swpins core ls` [*sw-pattern*]
  List core software packages used internally by confctl.

`confctl swpins core set` *sw-pattern* *version...*
  Set selected core software package to new *version*. The value
  of *version* depends on the type of the software pin, for git it is a git
  reference, e.g. a revision.

`confctl swpins core update` [*sw-pattern*]
  Update selected or all core software packages that have been configured
  to support this command. The usual case for git is to pin to the current
  branch head.

`confctl swpins update`
  Update software pins that have been configured for updates, including pins
  in all channels, all machine-specific pins and the core pins.

`confctl swpins reconfigure`
  Regenerate all confctl-managed software pin files according to the Nix
  configuration.

## SEE ALSO
confctl-options.nix(8)

## BUGS
Report bugs to https://github.com/vpsfreecz/confctl/issues.

## ABOUT
`confctl` was originally developed for the purposes of
[vpsFree.cz](https://vpsfree.org) and its cluster 
[configuration](https://github.com/vpsfreecz/vpsfree-cz-configuration).
