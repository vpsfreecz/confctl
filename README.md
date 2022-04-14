# confctl
confctl is a Nix deployment configuration management tool. It can be used to
build and deploy [NixOS](https://nixos.org) and [vpsAdminOS](https://vpsadminos.org)
machines.

## Features

* Stateless
* Per-machine nixpkgs (both modules and packages)
* Build generations for easy rollback
* Support for configuration interconnections (declare and access other machines'
  configurations)
* Query machine state, view changelogs and diffs

## Requirements

* [Nix](https://nixos.org)

## Quick start
1. There are no releases or packages yet, so first clone the repository:
```
git clone https://github.com/vpsfreecz/confctl
```

2. Create a new directory, where your confctl-managed configuration will be
stored:

```
mkdir cluster-configuration
```
3.Prepare `shell.nix` in the new directory, chose one option
  - Create a `shell.nix` and import the same file from confctl:
```
cd cluster-configuration
cat > shell.nix <<EOF
import ../confctl/shell.nix
EOF
```

  - Alternatively, you can symlink `shell.nix` from the confctl repository:
```
cd cluster-configuration
ln -s ../confctl/shell.nix shell.nix
```

4. Enter the `nix-shell`. This will make confctl available and install its
dependencies into `.gems/`:
```
nix-shell
```

From within the shell, you can access the [manual](./man/man8/confctl.8.md)
and a list of [configuration options](./man/man8/confctl-options.nix.8.md):

```
man confctl
man confctl-options.nix
```

5. Initialize the configuration directory with confctl:
```
confctl init
```

6. Add a new machine to be deployed:
```
confctl add my-machine
```

You can now edit the machine's configuration in directory `cluster/my-machine`.

7. Update pre-configured software pins to fetch current nixpkgs:
```
confctl swpins update
```

8. Build the machine
```
confctl build my-machine
```

9. Deploy the machine
```
confctl deploy my-machine
```

## Example configuration
Example configuration, which can be used as a starting point, can be found in
directory [example/](example/).

See also
[vpsfree-cz-configuration](https://github.com/vpsfreecz/vpsfree-cz-configuration)
for a full-featured cluster configuration.

## Configuration directory structure
`confctl` configurations should adhere to the following structure:

    cluster-configuration/      # Configuration root
    ├── cluster/                # Machine configurations
    │   ├── <name>/             # Single machine, can be nested directories
    │   │   ├── config.nix      # Standard NixOS system configuration
    │   │   └── module.nix      # Config with machine metadata used by confctl
    │   ├── cluster.nix         # confctl-generated list of machines
    │   └── module-list.nix     # List of all machine modules (including those in cluster.nix)
    ├── configs/                # confctl and other user-defined configs
    │   ├── confctl.nix         # Configuration for the confctl tool itself
    │   └── swpins.nix          # User-defined software pin channels
    ├── data/                   # User-defined datasets available in machine configurations as confData
    ├── environments/           # Environment presets for various types of machines, optional
    ├── modules/                # User-defined modules
    │   └── cluster/default.nix # User-defined extensions of `cluster.` options used in `<machine>/module.nix` files
    ├── swpins/                 # confctl-generated software pins configuration
    └── shell.nix               # Nix expression for nix-shell

## Software pins
Software pins in confctl allow you to use specific revisions of
[nixpkgs](https://github.com/NixOS/nixpkgs) or any other software to build
and deploy target machines. It doesn't matter what nixpkgs version the build
machine uses, because each machine gets its own nixpkgs as configured by the
software pin.

Software pins can be grouped in channels, which can then be used by all
or selected machines in the configuration. Or, if needed, custom software pins
can be configured on selected machines. See below for usage examples.

## Software pin channels
Software pin channels are defined in file `confctl/swpins.nix`:

```nix
{ config, ... }:
{
  confctl.swpins.channels = {
    # Channel for NixOS unstable
    nixos-unstable = {  # channel name
      nixpkgs = {  # swpin name
        # git-rev fetches the contents of a git commit and adds information
        # about the current git revision
        type = "git-rev";

        git-rev = {  # swpin type-specific configuration
          # Repository URL
          url = "https://github.com/NixOS/nixpkgs";

          # Fetch git submodules or not
          fetchSubmodules = false;

          # git reference to use for manual/automated update using
	  # `confctl swpins channel update`
          update.ref = "refs/heads/nixos-unstable";

          # Whether to enable automated updates triggered by `confctl build | deploy`
          update.auto = true;

          # If update.auto is true, this determines how frequently will confctl
          # try to update the channel, in seconds
          update.interval = 60*60;
	};
      };
    };

    # Channel for vpsAdminOS staging
    vpsadminos-staging = {
      vpsadminos = {
        type = "git-rev";

        git-rev = {
          url = "https://github.com/vpsfreecz/vpsadminos";
          update.ref = "refs/heads/staging";
	  update.auto = true;
        };
      };
    };
  };
}
```

This configuration defines what channels exist, what packages they contain
and how to fetch them. Such configured channels can then be manipulated using
`confctl`. `confctl` prefetches selected software pins and saves their hashes
in JSON files in the `swpins/` directory.

```
# List channels
$ confctl swpins channel ls
CHANNEL             SW           TYPE      PIN
nixos-unstable      nixpkgs      git-rev   1f77a4c8
vpsadminos-staging  vpsadminos   git-rev   9c9a7bcb

# Update channels with update.auto = true to reference in update.ref
$ confctl swpins channel update

# You can update only selected channels or swpins
$ confctl swpins channel update nixos-unstable
$ confctl swpins channel update nixos-unstable nixpkgs

# Set swpin to a custom git reference
$ confctl swpins channel set nixos-unstable nixpkgs 1f77a4c8
```

`confctl build` and `confctl deploy` will now use the prefetched software pins.

## Machine metadata and software pins
Machine configuration directory usually contains at least two files:
`cluster/<machine name>/config.nix` and `cluster/<machine name>/module.nix`.

`config.nix` is evaluated only when that particular machine is being built. It is
a standard NixOS configuration module, similar to `/etc/nixos/configuration.nix`.

`module.nix` is specific to confctl configurations. `module.nix` files from
all machines are evaluated during every build, whether that particular machine
is being built or not. `module.nix` contains metadata about machines from which
confctl knows how to treat them. It is also used to declare which software pins
or channels the machine uses. Metadata about any machine can be read from
`config.nix` of any other machine.

For example, machine named `my-machine` would be described in
`cluster/my-machine/module.nix` as:

```nix
{ config, ... }:
{
  cluster."my-machine" = {
    # This tells confctl whether it is a NixOS or vpsAdminOS machine
    spin = "nixos";

    # Use NixOS unstable channel defined in configs/swpins.nix
    swpins.channels = [ "nixos-unstable" ];

    # If the machine name is not a hostname, configure the address to which
    # should confctl deploy it
    host.target = "<ip address>";
  };
}
```

See [man/man8/confctl-options.nix.8.md](./man/man8/confctl-options.nix.8.md)
for a list of all options.

## Per-machine software pins
It is simpler to use software pins from channels, because they are usually
used by multiple machines, but it is possible to define per-machine software
pins, either to override pins from channels or add custom ones.

Per-machine software pins are configured in the machine's `module.nix` file:

```nix
{ config, ... }:
{
  cluster."my-machine" = {
    # List of channels
    swpins.channels = [ "..." ];

    # Per-machine swpins
    swpins.pins = {
      "pin-name" = {
          type = "git-rev";
	  git-rev = {
            # ...pin definition...
	  };
       };
    };
  };
}
```

The configuration is exactly the same as that of software pins in channels.
Instead of `confctl swpins channel` commands, use `confctl swpins cluster`
to manage configured pins.

## Extra module arguments
Machine configs can use the following extra module arguments:

- `confDir` - path to the cluster configuration directory
- `confLib` - confctl functions, see [nix/lib/default.nix](nix/lib/default.nix)
- `confData` - access to user-defined datasets found in `data/default.nix`,
  see [example/data/default.nix](example/data/default.nix)
- `confMachine` - attrset with information about the machine that is currently
  being built, contains key `name` and all options from
  [machine metadata module](##machine-metadata-and-software-pins)
- `swpins` - attrset of software pins of the machine that is currently being built

For example in `cluster/my-machine/config.nix`:

```nix
{ config, lib, pkgs, confLib, confData, confMachine, ... }:
{
  # Set the hostname to the machine name from confctl
  networking.hostName = confMachine.name;

  # When used with the data defined at example/data/default.nix
  users.users.root.openssh.authorizedKeys.keys = with confData.sshKeys; admins;
}
```

## confctl configuration
The `confctl` utility itself can be configured using `configs/confctl.nix`:

```nix
{ config, ... }:
{
  confctl = {
    # Columns that are shown by `confctl ls`. Any option from machine metadata
    # can be used.
    listColumns = {
      "name"
      "spin"
      "host.fqdn"
    };
  };
}
```

## Extending machine metadata
To define your own options to be used within the `cluster.<name>` modules in
`cluster/<machine>/module.nix` files, create file `modules/cluster/default.nix`,
e.g.:

```nix
{ config, lib, ... }:
with lib;
let
  myMachine =
    { config, ... }:
    {
      options = {
        myParameter = mkOption { ... };
      };
    };
in {
  options = {
    cluster = mkOption {
      type = types.attrsOf (types.submodule myMachine);
    };
  };
}
```

Then you can use it in machine module as:

```nix
cluster."my-machine" = {
  myParameter = "1234";
};
```

Note that these modules are self-contained. They are not evaluated with the full
set of NixOS modules. You have to import modules that you need.
