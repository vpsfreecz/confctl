# confctl 8                       2020-11-01                             master

## NAME
`confctl` - Nix deployment management tool

## SYNOPSIS
`confctl` [*global options*] *command* [*command options*] [*arguments...*]

## DESCRIPTION
`confctl` is a Nix deployment configuration management tool. It can be used to
build and deploy *NixOS* and *vpsAdminOS* machines.

## SOFTWARE PINS
Each deployment target managed by `confctl` uses predefined software packages
like `nixpkgs`, `vpsadminos` and possibly other components. These packages
are pinned to particular versions, e.g. specific git commits.

Software pins are defined in the Nix configuration and then prefetched using
`confctl`. Selected software pins are added to environment variable `NIX_PATH`
for `nix-build` and can also be read by `Nix` while building deployments.

Software pins can be defined using channels or on specific deployments.
The advantage of using channels is that changing a pin in a channel changes
also all deployments that use the channel. Channels are defined in file
`configs/swpins.nix` using option `confctl.swpins.channels`. Deployments
declare software pins and channels in their respective `module.nix` files,
option `cluster.<name>.swpins.channels` is a list of channels to use and option
`cluster.<name>.swpins.pins` is an attrset of pins to extend or override pins
from channels.

Software pins declared in the Nix configuration have to be prefetched before
they can be used to build deployments. See the `confctl swpins` command family
for more information.

## PATTERNS
`confctl` commands accept patterns instead of names. These patterns work
similarly to shell patterns, see
<http://ruby-doc.org/core/File.html#method-c-fnmatch-3F> for more
information.

## COMMANDS
`confctl init`
  Create a new configuration in the current directory. The current directory
  is set up to be used with `confctl`.

`confctl add` *name*
  Add new deployment.

`confctl rename` *old-name* *new-name*
  Rename deployment.

`confctl rediscover`
  Auto-discover deployments within the `cluster/` directory and generate a list
  of their modules in `cluster/cluster.nix`.

`confctl ls` [*options*] [*host-pattern*]
  List matching hosts available for deployment.

    `--show-trace`
      Enable traces in Nix.

    `--managed` `y`|`yes`|`n`|`no`|`a`|`all`
      The configuration can contain deployments which are not managed by confctl
      and are there just for reference. This option determines what kind of
      deployments should be listed.

    `-o`, `--output` *attributes*
      Comma-separated list of attributes to output. Defaults to the value
      of option `confctl.list.columns`.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter deployments by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter deployments that have *tag* set. If the tag begins with `^`, then
      filter deployments that do not have *tag* set.

`confctl build` [*options*] [*host-pattern*]
  Build matching hosts.

    `--show-trace`
      Enable traces in Nix.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter deployments by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter deployments that have *tag* set. If the tag begins with `^`, then
      filter deployments that do not have *tag* set.

    `-y`, `--yes`
      Do not ask for confirmation on standard input, assume the answer is yes.

`confctl deploy` [*options*] [*host-pattern*] [`boot`|`switch`|`test`|`dry-activate`]
  Build and deploy matching hosts. *switch-action* is the argument to
  `switch-to-configuration` called on the target host. The default action
  is `switch`.

    `--show-trace`
      Enable traces in Nix.

    `-a`, `--attr` *attribute*`=`*value* | *attribute*`!=`*value*
      Filter deployments by selected attribute, which is either tested for
      equality or inequality. Any attribute from configuration module
      `cluster.<name>` can be tested.

    `-t`, `--tag` *tag*|`^`*tag*
      Filter deployments that have *tag* set. If the tag begins with `^`, then
      filter deployments that do not have *tag* set.

    `-y`, `--yes`
      Do not ask for confirmation on standard input, assume the answer is yes.

    `-i`, `--interactive`
      Deploy hosts one by one while asking for confirmation for activation.

`confctl gen-data vpsadmin all`
  Generate all required data files from vpsAdmin API.

`confctl gen-data vpsadmin containers`
  Generate container data files from vpsAdmin API.

`confctl gen-data vpsadmin network`
  Generate network data files from vpsAdmin API.

`confctl swpins cluster ls` [*name-pattern* [*sw-pattern*]]
  List cluster deployments with pinned software packages.

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

`confctl swpins reconfigure`
  Regenerate all confctl-managed software pin files according to the Nix
  configuration.

## BUGS
Report bugs to https://github.com/vpsfreecz/confctl/issues.

## ABOUT
`confctl` was originally developed for the purposes of
[vpsFree.cz](https://vpsfree.org) and its cluster 
[configuration](https://github.com/vpsfreecz/vpsfree-cz-configuration).
