# confctl-options.nix 8           2024-05-06                             master

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

    *Default:* `15552000`

    *Declared by:* `<confctl/nix/modules/confctl/generations.nix>`

`confctl.buildGenerations.min`
  The minimum number of build generations to be kept.
  
  This is the default value, which can be overriden per host.

    *Type:* signed integer

    *Default:* `5`

    *Declared by:* `<confctl/nix/modules/confctl/generations.nix>`

`confctl.hostGenerations.collectGarbage`
  Run nix-collect-garbage

    *Type:* boolean

    *Default:* `true`

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

    *Default:* `15552000`

    *Declared by:* `<confctl/nix/modules/confctl/generations.nix>`

`confctl.hostGenerations.min`
  The minimum number of generations to be kept on machines.
  
  This is the default value, which can be overriden per host.

    *Type:* signed integer

    *Default:* `5`

    *Declared by:* `<confctl/nix/modules/confctl/generations.nix>`

`confctl.list.columns`
  Configure which columns should `confctl ls` show.
  Names correspond to options within `cluster.<name>`
  module.

    *Type:* list of string

    *Default:* `[
  "name"
  "spin"
  "host.fqdn"
]`

    *Declared by:* `<confctl/nix/modules/confctl/cli.nix>`

`confctl.nix.maxJobs`
  Maximum number of build jobs, passed to `nix-build`
  commands.

    *Type:* null or signed integer or value "auto" (singular enum)

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/confctl/nix.nix>`

`confctl.nix.nixPath`
  List of extra paths added to environment variable
  `NIX_PATH` for all `nix-build`
  invokations

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/confctl/nix.nix>`



## SOFTWARE PIN CHANNELS
The following `confctl` settings for software pin channels can be configured
in `configs/swpins.nix` within the deployment configuration directory:

`confctl.swpins.channels`
  Software pin channels

    *Type:* attribute set of attribute set of (submodule)

    *Default:* `{ }`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.directory`
  This option has no description.

    *Type:* null or (submodule)

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.directory.path`
  Absolute path to the directory

    *Type:* string

    *Default:* `null`

    *Example:* `"/opt/my-swpin"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.channels.<name>.<name>.git`
  This option has no description.

    *Type:* null or (submodule)

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

    *Type:* null or (submodule)

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

    *Type:* one of "directory", "git", "git-rev"

    *Default:* `"git"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.channels`
  List of channels from `confctl.swpins.channels`
  to use for core swpins

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins`
  Core software packages used internally by confctl
  
  It has to contain package `nixpkgs`, which is used
  to resolve other software pins from channels or cluster machines.

    *Type:* attribute set of (submodule)

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

`confctl.swpins.core.pins.<name>.directory`
  This option has no description.

    *Type:* null or (submodule)

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.directory.path`
  Absolute path to the directory

    *Type:* string

    *Default:* `null`

    *Example:* `"/opt/my-swpin"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`

`confctl.swpins.core.pins.<name>.git`
  This option has no description.

    *Type:* null or (submodule)

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

    *Type:* null or (submodule)

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

    *Type:* one of "directory", "git", "git-rev"

    *Default:* `"git"`

    *Declared by:* `<confctl/nix/modules/confctl/swpins.nix>`



## MACHINE CONFIGURATION
The following options can be configured in per-machine `module.nix` files within
the deployment configuration directory, i.e. `cluster/<machine-name>/module.nix`:

`cluster.<name>.addresses`
  IP addresses

    *Type:* null or (submodule)

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.addresses.primary`
  Default address other machines should use to connect to this machine
  
  Defaults to the first IPv4 address if not set

    *Type:* null or (submodule)

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

    *Type:* list of (submodule)

    *Default:* `[ ]`

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

    *Type:* list of (submodule)

    *Default:* `[ ]`

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

`cluster.<name>.buildAttribute`
  Path to the attribute in machine system config that should be built
  
  For example, `[ "system" "build" "toplevel" ]` will select attribute
  `config.system.build.toplevel`.

    *Type:* list of string

    *Default:* `[
  "system"
  "build"
  "toplevel"
]`

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

`cluster.<name>.carrier.enable`
  Whether to enable This machine is a carrier for other machines.

    *Type:* boolean

    *Default:* `false`

    *Example:* `true`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.carrier.machines`
  List of carried machines

    *Type:* list of (submodule)

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.carrier.machines.*.alias`
  Alias for carried machine name

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.carrier.machines.*.buildAttribute`
  Path to the attribute in machine system config that should be built
  
  For example, `[ "system" "build" "toplevel" ]` will select attribute
  `config.system.build.toplevel`.

    *Type:* list of string

    *Default:* `[
  "system"
  "build"
  "toplevel"
]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.carrier.machines.*.extraModules`
  A list of additional NixOS modules to be imported for this machine

    *Type:* list of path

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.carrier.machines.*.labels`
  Optional user-defined labels to classify the machine

    *Type:* attribute set

    *Default:* `{ }`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.carrier.machines.*.machine`
  Machine name

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.carrier.machines.*.tags`
  Optional user-defined tags to classify the machine

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands`
  Check commands run on the build machine

    *Type:* list of (submodule)

    *Default:* `[ ]`

    *Example:* `[
  { description = "ping"; command = [ "ping" "-c1" "{host.fqdn}" ]; }
]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.command`
  Command and its arguments
  
  It is possible to access machine attributes as from CLI using curly
  brackets. For example, {host.fqdn} would be replaced by machine FQDN.
  See confctl ls -L for a list of available attributes.

    *Type:* list of string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.cooldown`
  Number of seconds in between check attempts

    *Type:* unsigned integer, meaning >=0

    *Default:* `3`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.description`
  Command description

    *Type:* string

    *Default:* `""`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.exitStatus`
  Expected exit status

    *Type:* unsigned integer, meaning >=0

    *Default:* `0`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.standardError.exclude`
  String that must not be included in standard error

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.standardError.include`
  String that must be included in standard error

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.standardError.match`
  Standard error must match this string

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.standardOutput.exclude`
  Strings that must not be included in standard output

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.standardOutput.include`
  Strings that must be included in standard output

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.standardOutput.match`
  Standard output must match this string

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.builderCommands.*.timeout`
  Max number of seconds to wait for the check to pass

    *Type:* unsigned integer, meaning >=0

    *Default:* `60`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands`
  Check commands run on the target machine
  
  Note that the commands have to be available on the machine.

    *Type:* list of (submodule)

    *Default:* `[ ]`

    *Example:* `[
  { description = "curl"; command = [ "curl" "-s" "http://localhost:80" ]; }
]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.command`
  Command and its arguments
  
  It is possible to access machine attributes as from CLI using curly
  brackets. For example, {host.fqdn} would be replaced by machine FQDN.
  See confctl ls -L for a list of available attributes.

    *Type:* list of string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.cooldown`
  Number of seconds in between check attempts

    *Type:* unsigned integer, meaning >=0

    *Default:* `3`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.description`
  Command description

    *Type:* string

    *Default:* `""`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.exitStatus`
  Expected exit status

    *Type:* unsigned integer, meaning >=0

    *Default:* `0`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.standardError.exclude`
  String that must not be included in standard error

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.standardError.include`
  String that must be included in standard error

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.standardError.match`
  Standard error must match this string

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.standardOutput.exclude`
  Strings that must not be included in standard output

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.standardOutput.include`
  Strings that must be included in standard output

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.standardOutput.match`
  Standard output must match this string

    *Type:* null or string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.machineCommands.*.timeout`
  Max number of seconds to wait for the check to pass

    *Type:* unsigned integer, meaning >=0

    *Default:* `60`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.enable`
  Enable systemd checks, enabled by default

    *Type:* boolean

    *Default:* `true`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.systemProperties`
  Check systemd manager properties reported by systemctl show

    *Type:* list of (submodule)

    *Default:* `[
  {
    property = "SystemState";
    value = "running";
  }
]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.systemProperties.*.cooldown`
  Number of seconds in between check attempts

    *Type:* unsigned integer, meaning >=0

    *Default:* `3`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.systemProperties.*.property`
  systemd property name

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.systemProperties.*.timeout`
  Max number of seconds to wait for the check to pass

    *Type:* unsigned integer, meaning >=0

    *Default:* `60`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.systemProperties.*.value`
  value to be checked

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.unitProperties`
  Check systemd unit properties reported by systemctl show <unit>

    *Type:* attribute set of list of (submodule)

    *Default:* `{ }`

    *Example:* `{
  "firewall.service" = [
    { property = "ActiveState"; value = "active"; }
  ];
}`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.unitProperties.<name>.*.cooldown`
  Number of seconds in between check attempts

    *Type:* unsigned integer, meaning >=0

    *Default:* `3`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.unitProperties.<name>.*.property`
  systemd property name

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.unitProperties.<name>.*.timeout`
  Max number of seconds to wait for the check to pass

    *Type:* unsigned integer, meaning >=0

    *Default:* `60`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.healthChecks.systemd.unitProperties.<name>.*.value`
  value to be checked

    *Type:* string

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.host`
  This option has no description.

    *Type:* null or (submodule)

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

`cluster.<name>.hostGenerations.collectGarbage`
  Run nix-collect-garbage

    *Type:* null or boolean

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

    *Default:* `{ }`

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

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.nix.nixPath`
  List of extra paths added to environment variable
  `NIX_PATH` for `nix-build`

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.spin`
  OS type

    *Type:* one of "openvz", "nixos", "vpsadminos", "other"

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.channels`
  List of channels from `confctl.swpins.channels`
  to use on this machine

    *Type:* list of string

    *Default:* `[ ]`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins`
  List of swpins for this machine, which can supplement or
  override swpins from configured channels

    *Type:* attribute set of (submodule)

    *Default:* `{ }`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.directory`
  This option has no description.

    *Type:* null or (submodule)

    *Default:* `null`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.directory.path`
  Absolute path to the directory

    *Type:* string

    *Default:* `null`

    *Example:* `"/opt/my-swpin"`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.swpins.pins.<name>.git`
  This option has no description.

    *Type:* null or (submodule)

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

    *Type:* null or (submodule)

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

    *Type:* one of "directory", "git", "git-rev"

    *Default:* `"git"`

    *Declared by:* `<confctl/nix/modules/cluster>`

`cluster.<name>.tags`
  Optional user-defined tags to classify the machine

    *Type:* list of string

    *Default:* `[ ]`

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
