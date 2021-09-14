# confctl-options.nix 8           2021-07-10                             master

## NAME
`confctl-options.nix` - confctl configuration documentation

## DESCRIPTION
This document describes Nix options, which can be used in confctl(8) cluster
configurations to configure `confctl` and machines within the cluster.

## CONFCTL SETTINGS
The following `confctl` settings can be configured in `configs/confctl.nix`
within the deployment configuration directory:

`confctl.buildGenerations.max`
  The maximum number of build generations to be kept.
  
  This is the default value, which can be overriden per host.

    *Type:* signed integer

    *Default:* `30`

    *Declared by:* `<confctl/nix/modules/confctl/generations.nix>`

`confctl.buildGenerations.maxAge`
  Delete build generations older than
  `confctl.buildGenerations.maxAge` seconds. Old generations
  are deleted even if `confctl.buildGenerations.max` is
  not reached.
  
  This is the default value, which can be overriden per host.

    *Type:* signed integer

    *Default:* `7776000`

    *Declared by:* `<confctl/nix/modules/confctl/generations.nix>`

`confctl.buildGenerations.min`
  The minimum number of build generations to be kept.
  
  This is the default value, which can be overriden per host.

    *Type:* signed integer

    *Default:* `4`

    *Declared by:* `<confctl/nix/modules/confctl/generations.nix>`

`confctl.hostGenerations.max`
  The maximum number of generations to be kept on machines.
  
  This is the default value, which can be overriden per host.

    *Type:* signed integer

    *Default:* `30`

    *Declared by:* `<confctl/nix/modules/confctl/generations.nix>`

`confctl.hostGenerations.maxAge`
  Delete generations older than
  `confctl.hostGenerations.maxAge` seconds from
  machines. Old generations
  are deleted even if `confctl.hostGenerations.max` is
  not reached.
  
  This is the default value, which can be overriden per host.

    *Type:* signed integer

    *Default:* `7776000`

    *Declared by:* `<confctl/nix/modules/confctl/generations.nix>`

`confctl.hostGenerations.min`
  The minimum number of generations to be kept on machines.
  
  This is the default value, which can be overriden per host.

    *Type:* signed integer

    *Default:* `4`

    *Declared by:* `<confctl/nix/modules/confctl/generations.nix>`

`confctl.list.columns`
  Configure which columns should `confctl ls` show.
  Names correspond to options within `cluster.<name>`
  module.

    *Type:* list of strings

    *Default:* `["host.fqdn" "name" "spin"]`

    *Declared by:* `<confctl/nix/modules/confctl/cli.nix>`

`confctl.nix.maxJobs`
  Maximum number of build jobs, passed to `nix-build`
  commands.

    *Type:* null or signed integer or one of "auto"

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/confctl/nix.nix>`

`confctl.nix.nixPath`
  List of extra paths added to environment variable
  `NIX_PATH` for all `nix-build`
  invokations

    *Type:* list of strings

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/confctl/nix.nix>`



## SOFTWARE PIN CHANNELS
The following `confctl` settings for software pin channels can be configured
in `configs/swpins.nix` within the deployment configuration directory:

`confctl.swpins.channels`
  Software pin channels

    *Type:* attribute set of attribute set of submoduless

    *Default:* `{
}`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git.fetchSubmodules`
  Fetch git submodules

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git.update.auto`
  When enabled, the pin is automatically updated to
  `ref` before building machines.

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git.update.interval`
  Number of seconds from the last update to trigger the next
  auto-update, if auto-update is enabled.

    *Type:* signed integer

    *Default:* `3600`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git.update.ref`
  Implicit git reference to use for both manual and automatic updates

    *Type:* null or string

    *Default:* `null`

    *Example:* `"refs/heads/master"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git.url`
  URL of the git repository

    *Type:* string

    *Default:* `null`

    *Example:* `"https://github.com/vpsfreecz/vpsadminos"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git-rev`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git-rev.fetchSubmodules`
  Fetch git submodules

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git-rev.update.auto`
  When enabled, the pin is automatically updated to
  `ref` before building machines.

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git-rev.update.interval`
  Number of seconds from the last update to trigger the next
  auto-update, if auto-update is enabled.

    *Type:* signed integer

    *Default:* `3600`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git-rev.update.ref`
  Implicit git reference to use for both manual and automatic updates

    *Type:* null or string

    *Default:* `null`

    *Example:* `"refs/heads/master"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git-rev.url`
  URL of the git repository

    *Type:* string

    *Default:* `null`

    *Example:* `"https://github.com/vpsfreecz/vpsadminos"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.type`
  This option has no description.

    *Type:* one of "git", "git-rev"

    *Default:* `"git"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.channels`
  List of channels from `confctl.swpins.channels`
  to use for core swpins

    *Type:* list of strings

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins`
  Core software packages used internally by confctl
  
  It has to contain package `nixpkgs`, which is used
  to resolve other software pins from channels or cluster machines.

    *Type:* attribute set of submodules

    *Default:* `{
  nixpkgs = {
    git-rev = {
      update = {
        auto = true;
        interval = 2592000;
        ref = "refs/heads/nixos-unstable";
      };
      url = "https://github.com/NixOS/nixpkgs";
    };
    type = "git-rev";
  };
}`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git.fetchSubmodules`
  Fetch git submodules

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git.update.auto`
  When enabled, the pin is automatically updated to
  `ref` before building machines.

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git.update.interval`
  Number of seconds from the last update to trigger the next
  auto-update, if auto-update is enabled.

    *Type:* signed integer

    *Default:* `3600`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git.update.ref`
  Implicit git reference to use for both manual and automatic updates

    *Type:* null or string

    *Default:* `null`

    *Example:* `"refs/heads/master"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git.url`
  URL of the git repository

    *Type:* string

    *Default:* `null`

    *Example:* `"https://github.com/vpsfreecz/vpsadminos"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git-rev`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git-rev.fetchSubmodules`
  Fetch git submodules

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git-rev.update.auto`
  When enabled, the pin is automatically updated to
  `ref` before building machines.

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git-rev.update.interval`
  Number of seconds from the last update to trigger the next
  auto-update, if auto-update is enabled.

    *Type:* signed integer

    *Default:* `3600`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git-rev.update.ref`
  Implicit git reference to use for both manual and automatic updates

    *Type:* null or string

    *Default:* `null`

    *Example:* `"refs/heads/master"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git-rev.url`
  URL of the git repository

    *Type:* string

    *Default:* `null`

    *Example:* `"https://github.com/vpsfreecz/vpsadminos"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.type`
  This option has no description.

    *Type:* one of "git", "git-rev"

    *Default:* `"git"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`



## MACHINE CONFIGURATION
The following options can be configured in per-machine `module.nix` files within
the deployment configuration directory, i.e. `cluster/<machine-name>/module.nix`:

`cluster.<name>.addresses`
  IP addresses

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.primary`
  Default address other machines should use to connect to this machine
  
  Defaults to the first IPv4 address if not set

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.primary.address`
  IPv4 address

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.primary.prefix`
  Prefix length

    *Type:* positive integer, meaning >0

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.primary.string`
  Address with prefix as string

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.v4`
  List of IPv4 addresses this machine responds to

    *Type:* list of submodules

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.v4.*.address`
  IPv4 address

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.v4.*.prefix`
  Prefix length

    *Type:* positive integer, meaning >0

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.v4.*.string`
  Address with prefix as string

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.v6`
  List of IPv6 addresses this machine responds to

    *Type:* list of submodules

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.v6.*.address`
  IPv6 address

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.v6.*.prefix`
  Prefix length

    *Type:* positive integer, meaning >0

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.v6.*.string`
  Address with prefix as string

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.buildGenerations.max`
  The maximum number of build generations to be kept on the build
  machine.

    *Type:* null or signed integer

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.buildGenerations.maxAge`
  Delete build generations older than
  `cluster.<name>.buildGenerations.maxAge`
  seconds from the build machine. Old generations are deleted even
  if `cluster.<name>.buildGenerations.max` is
  not reached.

    *Type:* null or signed integer

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.buildGenerations.min`
  The minimum number of build generations to be kept on the build
  machine.

    *Type:* null or signed integer

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.host`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.host.domain`
  Host domain

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.host.fqdn`
  Host FQDN

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.host.fullDomain`
  Domain including location, i.e. FQDN without host name

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.host.location`
  Host location domain

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.host.name`
  Host name

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.host.target`
  Address/host to which the configuration is deployed to

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.hostGenerations.max`
  The maximum number of generations to be kept on the machine.

    *Type:* null or signed integer

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.hostGenerations.maxAge`
  Delete generations older than
  `cluster.<name>.hostGenerations.maxAge`
  seconds from the machine. Old generations are deleted even
  if `cluster.<name>.hostGenerations.max` is
  not reached.

    *Type:* null or signed integer

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.hostGenerations.min`
  The minimum number of generations to be kept on the machine.

    *Type:* null or signed integer

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.labels`
  Optional user-defined labels to classify the machine

    *Type:* attribute set

    *Default:* `{
}`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.managed`
  Determines whether the machine is managed using confctl or not
  
  By default, NixOS and vpsAdminOS machines are managed by confctl.

    *Type:* null or boolean

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.netboot.enable`
  Whether to enable Include this system on pxe servers.

    *Type:* boolean

    *Default:* `false`

    *Example:* `true`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.netboot.macs`
  List of MAC addresses for iPXE node auto-detection

    *Type:* list of strings

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.nix.nixPath`
  List of extra paths added to environment variable
  `NIX_PATH` for `nix-build`

    *Type:* list of strings

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.spin`
  OS type

    *Type:* one of "openvz", "nixos", "vpsadminos", "other"

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.channels`
  List of channels from `confctl.swpins.channels`
  to use on this machine

    *Type:* list of strings

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins`
  List of swpins for this machine, which can supplement or
  override swpins from configured channels

    *Type:* attribute set of submodules

    *Default:* `{
}`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git.fetchSubmodules`
  Fetch git submodules

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git.update.auto`
  When enabled, the pin is automatically updated to
  `ref` before building machines.

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git.update.interval`
  Number of seconds from the last update to trigger the next
  auto-update, if auto-update is enabled.

    *Type:* signed integer

    *Default:* `3600`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git.update.ref`
  Implicit git reference to use for both manual and automatic updates

    *Type:* null or string

    *Default:* `null`

    *Example:* `"refs/heads/master"`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git.url`
  URL of the git repository

    *Type:* string

    *Default:* `null`

    *Example:* `"https://github.com/vpsfreecz/vpsadminos"`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git-rev`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git-rev.fetchSubmodules`
  Fetch git submodules

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git-rev.update.auto`
  When enabled, the pin is automatically updated to
  `ref` before building machines.

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git-rev.update.interval`
  Number of seconds from the last update to trigger the next
  auto-update, if auto-update is enabled.

    *Type:* signed integer

    *Default:* `3600`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git-rev.update.ref`
  Implicit git reference to use for both manual and automatic updates

    *Type:* null or string

    *Default:* `null`

    *Example:* `"refs/heads/master"`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git-rev.url`
  URL of the git repository

    *Type:* string

    *Default:* `null`

    *Example:* `"https://github.com/vpsfreecz/vpsadminos"`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.type`
  This option has no description.

    *Type:* one of "git", "git-rev"

    *Default:* `"git"`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.tags`
  Optional user-defined tags to classify the machine

    *Type:* list of strings

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/cluster>`



## SERVICES
The following options can be configured in per-machine `config.nix` files within
the deployment configuration directory, i.e. `cluster/<machine-name>/config.nix`,
or any other imported Nix file. These options are added by `confctl` in addition
to options from `NixOS` or `vpsAdminOS`.



## SEE ALSO
confctl(8)

## BUGS
Report bugs to https://github.com/vpsfreecz/confctl/issues.

## ABOUT
`confctl` was originally developed for the purposes of
[vpsFree.cz](https://vpsfree.org) and its cluster
[configuration](https://github.com/vpsfreecz/vpsfree-cz-configuration).
