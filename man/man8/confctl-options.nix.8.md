# confctl-options.nix 8           2021-04-23                             master

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

`cluster.<name>.container`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.container.id`
  VPS ID in vpsAdmin

    *Type:* signed integer

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

`cluster.<name>.logging.enable`
  Send logs to central log system

    *Type:* boolean

    *Default:* `true`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.logging.isLogger`
  This system is used as a central log system

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.managed`
  Determines whether the machine is managed using confctl or not
  
  By default, NixOS and vpsAdminOS machines are managed by confctl.

    *Type:* null or boolean

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.monitoring.enable`
  Monitor this system

    *Type:* boolean

    *Default:* `true`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.monitoring.isMonitor`
  Determines if this system is monitoring other systems, or if it
  is just being monitored

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.monitoring.labels`
  Custom labels added to the Prometheus target

    *Type:* attribute set

    *Default:* `{
}`

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

`cluster.<name>.node`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.node.id`
  ID of this node in vpsAdmin

    *Type:* null or positive integer, meaning >0

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.node.role`
  Node role

    *Type:* one of "hypervisor", "storage"

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.bird.as`
  BGP AS for this node

    *Type:* positive integer, meaning >0

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.bird.bfdInterfaces`
  BFD interfaces match

    *Type:* string

    *Default:* `"teng*"`

    *Example:* `"teng*"`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.bird.bgpNeighbours.v4`
  IPv4 BGP neighbour addresses

    *Type:* list of submodules

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.bird.bgpNeighbours.v4.*.address`
  IPv4 address

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.bird.bgpNeighbours.v4.*.as`
  BGP AS

    *Type:* signed integer

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.bird.bgpNeighbours.v6`
  IPv6 BGP neighbour addresses

    *Type:* list of submodules

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.bird.bgpNeighbours.v6.*.address`
  IPv6 address

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.bird.bgpNeighbours.v6.*.as`
  BGP AS

    *Type:* signed integer

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.bird.enable`
  Enable BGP routing using bird

    *Type:* boolean

    *Default:* `true`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.bird.routerId`
  bird router ID

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.interfaces.addresses`
  List of addresses which are added to interfaces

    *Type:* attribute set of submodules

    *Default:* `{
}`

    *Example:* `{
  teng0 = {
    v4 = ["1.2.3.4/32"];
    v6 = [];
  };
}`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.interfaces.addresses.<name>.v4`
  A lisf of IPv4 addresses with prefix

    *Type:* list of submodules

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.interfaces.addresses.<name>.v4.*.address`
  IPv4 address

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.interfaces.addresses.<name>.v4.*.prefix`
  Prefix length

    *Type:* positive integer, meaning >0

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.interfaces.addresses.<name>.v4.*.string`
  Address with prefix as string

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.interfaces.addresses.<name>.v6`
  A lisf of IPv6 addresses with prefix

    *Type:* list of submodules

    *Default:* `[]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.interfaces.addresses.<name>.v6.*.address`
  IPv4 address

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.interfaces.addresses.<name>.v6.*.prefix`
  Prefix length

    *Type:* positive integer, meaning >0

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.interfaces.addresses.<name>.v6.*.string`
  Address with prefix as string

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.interfaces.names`
  Ensure network interface names based on MAC addresses

    *Type:* attribute set of strings

    *Default:* `{
}`

    *Example:* `{
  teng0 = "00:11:22:33:44:55";
}`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.virtIP`
  Virtual IP for dummy interface

    *Type:* null or submodule

    *Default:* `null`

    *Example:* `{
  address = "10.0.0.100";
  prefix = 32;
}`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.virtIP.address`
  IPv4 address

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.virtIP.prefix`
  Prefix length

    *Type:* positive integer, meaning >0

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.networking.virtIP.string`
  Address with prefix as string

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.serial.baudRate`
  Serial baudrate

    *Type:* positive integer, meaning >0

    *Default:* `115200`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.osNode.serial.enable`
  Whether to enable Enable serial console output.

    *Type:* boolean

    *Default:* `false`

    *Example:* `true`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.services`
  Services published by this machine

    *Type:* attribute set of submodules

    *Default:* `{
}`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.services.<name>.address`
  Address that other machines can access the service on

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.services.<name>.monitor`
  What kind of monitoring this services needs

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.services.<name>.port`
  Port the service listens on

    *Type:* null or signed integer

    *Default:* `null`

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

`cluster.<name>.vzNode`
  This option has no description.

    *Type:* null or submodule

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.vzNode.role`
  Node role

    *Type:* one of "hypervisor", "storage"

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`



## SERVICES
The following options can be configured in per-machine `config.nix` files within
the deployment configuration directory, i.e. `cluster/<machine-name>/config.nix`,
or any other imported Nix file. These options are added by `confctl` in addition
to options from `NixOS` or `vpsAdminOS`.

`services.netboot.acmeSSL`
  Enable ACME and SSL for netboot host

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`

`services.netboot.allowedIPRanges`
  Allow HTTP access for these IP ranges, if not specified
  access is not restricted.

    *Type:* list of strings

    *Default:* `[]`

    *Example:* `"10.0.0.0/24"`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`

`services.netboot.banner`
  Message to display on ipxe script load

    *Type:* string

    *Default:* `"ipxe loading"`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`

`services.netboot.enable`
  Whether to enable Enable netboot server.

    *Type:* boolean

    *Default:* `false`

    *Example:* `true`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`

`services.netboot.extraMappings`
  This option has no description.

    *Type:* attribute set of strings

    *Default:* `{
}`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`

`services.netboot.host`
  Hostname or IP address of the netboot server

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`

`services.netboot.includeNetbootxyz`
  Include netboot.xyz entry

    *Type:* boolean

    *Default:* `false`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`

`services.netboot.nixosItems`
  This option has no description.

    *Type:* attribute set of unspecifieds

    *Default:* `{
}`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`

`services.netboot.password`
  IPXE menu password

    *Type:* string

    *Default:* `"letmein"`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`

`services.netboot.secretsDir`
  Directory containing signing secrets

    *Type:* path

    *Default:* `"/nix/store/rsl9ffhzw9zv071rvqbny97bdv36rzr8-ca"`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`

`services.netboot.vpsadminosItems`
  This option has no description.

    *Type:* attribute set of unspecifieds

    *Default:* `{
}`

    *Declared by:* `<confctl/nix/modules/services/netboot.nix>`



## SEE ALSO
confctl(8)

## BUGS
Report bugs to https://github.com/vpsfreecz/confctl/issues.

## ABOUT
`confctl` was originally developed for the purposes of
[vpsFree.cz](https://vpsfree.org) and its cluster
[configuration](https://github.com/vpsfreecz/vpsfree-cz-configuration).
