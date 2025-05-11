# confctl
confctl is a Nix deployment configuration management tool. It can be used to
build and deploy [NixOS](https://nixos.org) and [vpsAdminOS](https://vpsadminos.org)
machines.

## Features

* Stateless
* Per-machine nixpkgs (both modules and packages)
* Build generations for easy rollback
* Rotation of old generations
* Support for configuration interconnections (declare and access other machines'
  configurations)
* Query machine state, view changelogs and diffs
* Run health checks
* Automatically roll back faulty configurations
* Support for creating netboot servers with option to kexec, see [docs/carrier.md](docs/carrier.md)

## Requirements

* [Nix](https://nixos.org)

## Quick start
1. Either install confctl as a gem:
```
gem install confctl
```

Or clone this repository:

```
git clone https://github.com/vpsfreecz/confctl
```

This guide assumes you have cloned the repository, because otherwise man will
not find confctl's manual pages. If you install confctl using gem, you can
ignore steps with `shell.nix`.

2. Create a new directory, where your confctl-managed configuration will be
stored:

```
mkdir cluster-configuration
```
3. Create `shell.nix` and import the same file from confctl:
```
cd cluster-configuration
cat > shell.nix <<EOF
import ../confctl/shell.nix
EOF
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

See also existing configurations:

* [vpsfree-cz-configuration](https://github.com/vpsfreecz/vpsfree-cz-configuration)
* [vpsadminos-org-configuration](https://github.com/vpsfreecz/vpsadminos-org-configuration)

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
    ├── scripts/                # User-defined scripts
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

## Nix flakes
confctl's software pins are an alternative to flakes and flakes are not supported
by confctl at this time. Software pins are implemented by manipulating the `$NIX_PATH`
environment variable, which is in conflict with using flakes. confctl is likely
to be migrated to flakes when the interface will be stabilized. Since the transition
is going to require a significant effort, there are no plans for it currently.

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

## Health checks
Health checks can be used to verify that the deployed systems behave correctly,
all services are running, etc. Health checks are run automatically after deploy
and can also be run on demand using `confctl health-check`. Health checks are
configured in machine metadata module, i.e. in `cluster/<machine>/module.nix`
files.

```nix
{ config, ... }:
{
  cluster."my-machine" = {
    # [...]

    healthChecks = {
      # Check that there are no failed units (this is actually done automatically
      # by confctl, you don't need to do this yourself)
      systemd.systemProperties = [
        { property = "SystemState"; value = "running"; }
      ];

      # Check that the firewall is active, we can check any property of any service
      systemd.unitProperties."firewall.service" = [
        { property = "ActiveState"; value = "active"; }
      ];

      # Run arbitrary commands from the builder
      builderCommands = [
        # Ping the deployed machine
        { command = [ "ping" "-c1" "{host.fqdn}" ]; }
      ];

      # Run commands on the deployed machine
      machineCommands = [
        # Try to access a fictional internal web server
        { command = [ "curl" "-s" "http://localhost:80" ]; }

        # We can also check command output
        { command = [ "hostname" ]; standardOutput.match = "my-machine\n"; }
      ];
    };
  };
}
```

## Rotate build generations
confctl can be used to rotate old generations both on the build machine
and on the deployed machines.

Default rotation settings can be set in confctl settings at `configs/confctl.nix`:

```nix
{ config, lib, ... }:
with lib;
{
  confctl = {
    # Generations on the build machine
    buildGenerations = {
      # Keep at least 4 generations
      min = mkDefault 4;

      # Do not keep more than 10 generations
      max = mkDefault 10;

      # Delete generations older than 90 days
      maxAge = mkDefault (90*24*60*60);
    };

    # The same settings can be configured for generations on the deployed machines
    hostGenerations = {
      min = mkDefault 40;
      max = mkDefault 100;
      maxAge = mkDefault (180*24*60*60);

      # On the deployed machines, confctl can also run nix-collect-garbage to
      # delete unreachable store paths
      collectGarbage = mkDefault true;
    };
  };
}
```

If these settings are not set, confctl uses its own defaults. Further, rotation
settings can be configured on per-machine basis in machine metadata module
at `cluster/<machine>/module.nix`:

```nix
{ config, ... }:
{
  cluster."my-machine" = {
    # [...]

    buildGenerations = {
      min = 8;
      max = 16;
    };

    hostGenerations = {
      min = 80;
    };
  };
}
```

Settings from the machine metadata modules override default confctl settings
from `configs/confctl.nix`.

To rotate the generations both on the build and deployed machines, run:

```
confctl generation rotate --local --remote
```

Generations can also be deleted manually, e.g. to delete generations older than
90 days, run:

```
confctl generation rm --local --remote '*' 30d
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
{ config, ... }:
{
  cluster."my-machine" = {
    myParameter = "1234";
  };
}
```

Note that these modules are self-contained. They are not evaluated with the full
set of NixOS modules. You have to import modules that you need.

## User-defined confctl commands
User-defined Ruby scripts can be placed in directory `scripts`. Each script
should create a subclass of `ConfCtl::UserScript` and call class-method `register`.
Scripts can define their own `confctl` subcommands.

### Example user script

```ruby
class MyScript < ConfCtl::UserScript
  register

  def setup_cli(app)
    app.desc 'My CLI command'
    app.command 'my-command' do |c|
      c.action &ConfCtl::Cli::Command.run(c, MyCommand, :run)
    end
  end
end

class MyCommand < ConfCtl::Cli::Command
  def run
    puts 'Hello world'
  end
end
```

## More information
See the [man pages](./man/man8) for more information:

* [confctl(8)](./man/man8/confctl.8.md)
* [confctl-options.nix(8)](./man/man8/confctl-options.nix.8.md)
