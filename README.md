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
### Flake configuration

confctl requires a configuration repository with `flake.nix` and the outputs from
`confctl.lib.mkConfctlOutputs`. The repository
defines flake inputs (nixpkgs/vpsadminos/etc.), maps them into channels, and
machines select channels via `cluster.<name>.inputs.channels`.

Create a new configuration directory and initialize it:

```bash
mkdir cluster-configuration
cd cluster-configuration
confctl init
```

`confctl init` generates a `flake.nix` like this:

```nix
{
  description = "confctl configuration (flake)";

  inputs = {
    confctl.url = "github:vpsfreecz/confctl";

    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # vpsadminos.url = "github:vpsfreecz/vpsadminos/staging";
    # vpsadminos.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = inputs@{ self, confctl, ... }:
    let
      channels = {
        nixos-unstable = { nixpkgs = "nixpkgs"; };

        # vpsadminos = {
        #   nixpkgs = "nixpkgs";
        #   vpsadminos = "vpsadminos";
        # };
      };

      confctlOutputs = confctl.lib.mkConfctlOutputs {
        confDir = ./.;
        inherit inputs channels;
      };
    in
    {
      confctl = confctlOutputs;

      # Shell modes:
      # - minimal: no Gemfile, best default
      # - tools: Gemfile for repo tools such as overcommit or rubocop
      devShells.x86_64-linux.default = confctl.lib.mkConfigDevShell {
        system = "x86_64-linux";
        mode = "minimal";
      };
    };
}
```

The optional `vpsadminos` input is commented out by default because most new
configurations deploy NixOS only.

Then enter the dev shell:

```bash
nix develop
```

This makes `confctl` available from the pinned flake input package and exposes
its man pages on `MANPATH`.

In tools mode, commit the configuration repository's `Gemfile.lock` to make
its Ruby tools reproducible. Those gems are separate from the packaged Rust
CLI. `bundled-confctl` mode is no longer supported.

Before the first build or deploy, make sure the per-user gcroot directory
exists:

```bash
sudo mkdir -p /nix/var/nix/gcroots/per-user/$USER
sudo chown $USER /nix/var/nix/gcroots/per-user/$USER
```

Add a new machine to be deployed:

```bash
confctl add my-machine
```

You can now edit the machine's configuration in directory `cluster/my-machine`.

Build the machine:

```bash
confctl build my-machine
```

Deploy the machine:

```bash
confctl deploy my-machine
```

To update flake inputs, use the `confctl inputs ...` commands:

```bash
confctl inputs ls
confctl inputs update --commit nixpkgs
confctl inputs channel update --commit nixos-unstable nixpkgs
```

For configurations created with software pins, migrate using confctl v3 before
updating the tool. The [upgrade guide](docs/swpins-to-flakes.md) describes migration,
retained generations, GC roots and rollback preparation. This version has no
software-pin or migration commands.

The [command manual](man/man8/confctl.8.md) and
[option reference](man/man8/confctl-options.nix.8.md) are also installed as man pages.
The package also installs a [CLI reference](man/man8/confctl-reference.8.md)
and the [kexec-netboot manual](man/man8/kexec-netboot.8.md). A Cargo example
[maintains the references](docs/rust-packaging.md#reference-maintenance) from
the CLI schema or an explicit options JSON export.

For development of confctl itself, enter this repository's `nix develop` shell
and install the repository hook:

```sh
git config --local core.hooksPath .githooks
```

The pre-commit hook checks Rust and tracked Nix source formatting without
staging changes. Run it in the declared shell so `cargo`, `rustfmt` and `nixfmt`
are available. It replaces the old Overcommit hook; install it in each clone.
The optional `shell.nix` uses host `<nixpkgs>` for the same development workflow.

`bin/confctl` runs the existing `confctl-rust` Cargo target with the source
manifest. It preserves the caller's configuration directory, arguments and
exit status. Use the declared shell first; Cargo can compile before starting
the command. Installed package wrappers execute their compiled backend directly.

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
    │   └── confctl.nix         # Configuration for the confctl tool itself
    ├── data/                   # User-defined datasets available in machine configurations as confData
    ├── environments/           # Environment presets for various types of machines, optional
    ├── flake.nix               # Flake entrypoint
    ├── flake.lock              # Locked input sources
    ├── Gemfile                 # Optional Ruby tools, used only in tools mode
    ├── Gemfile.lock            # Recommended for tools mode
    └── modules/                # User-defined modules
        └── cluster/default.nix # User-defined extensions of `cluster.` options used in `<machine>/module.nix` files

## Inputs and channels

Dependencies are flake inputs locked in `flake.lock`. A channel maps dependency
roles, such as `nixpkgs`, to root input names. Machines select channels through
`cluster.<name>.inputs.channels`; later channels override earlier roles.
Per-machine `inputs.overrides` takes precedence over all selected channels.

```nix
channels = {
  production = { nixpkgs = "nixpkgsStable"; };
  staging = { nixpkgs = "nixpkgsUnstable"; };
};
```

Manage the mapped inputs with:

```bash
confctl inputs channel ls
confctl inputs channel update --commit production nixpkgs
confctl inputs channel set --commit production nixpkgs <revision>
```

Builds and deployments use the existing lock without updating inputs automatically. See [Flake inputs](docs/flake-inputs.md)
for raw-source inputs, `follows`, path inputs and the optional NIX_PATH bridge.
`confctl` supports all six root/channel/machine update and
set routes; the guide describes selection, optional Git commits and partial-failure recovery.

`confctl` supports [deployment](docs/deploy.md) from
new builds or saved generations across boot, dry-activate, test and switch,
with optional reboot/wait-online for boot. Its default [status](docs/status.md) builds a target before
comparison; explicit saved/none modes remain read-only. Both installed CLI
names select the Rust implementation; site and terminal acceptance are separate.
[`diff` and `changelog`](docs/changes.md) compare inputs without building systems;
changelog can clone or fetch Git mirrors. Reports support redirected stdout and
an optional [pager](docs/pager.md).
[`confctl init`](docs/config-init.md) creates the existing starter
templates without an existing flake, Nix evaluation or confirmation. Its guide
lists permitted existing entries and recovery from partial initialization.
[`add`, `rename` and `rediscover`](docs/configuration-commands.md) edit an existing
flake configuration without confirmation or host contact. Their guide covers
literal templates, preserved rename contents, path restrictions, netboot hook
migration and recovery after partial edits.
[`generation rm`, `generation rotate` and `collect-garbage`](docs/generation-cleanup.md)
remove saved records and profiles and collect store objects. Their guide explains
retention, GC policy and recovery limits after irreversible partial cleanup.
[`gen-data vpsadmin`](docs/gen-data.md) authenticates and replaces local container
and network Nix data. All 30 parsed action leaves have concrete Rust dispatch.
Native package and site acceptance remain separate from these route contracts.

The [package and helper backends](docs/rust-packaging.md) are selected by
`packages.<system>.confctl` and its `confctl-rust` alias; the existing
`apps.<system>.confctl-rust` remains available. Configuration shells default to
`implementation = "rust"` and outputs default to `rustHelpers = true`.
Explicit Ruby selection, disabled Rust helpers, `bundled-confctl` mode and
`legacyCompat = true` fail with migration instructions. Minimal and tools
modes remain supported. Package builds, runtime closures and both target
architectures require native qualification.

## Machine metadata and inputs
Machine configuration directory usually contains at least two files:
`cluster/<machine name>/config.nix` and `cluster/<machine name>/module.nix`.

`config.nix` is evaluated only when that particular machine is being built. It is
a standard NixOS configuration module, similar to `/etc/nixos/configuration.nix`.

`module.nix` is specific to confctl configurations. `module.nix` files from
all machines are evaluated during every build, whether that particular machine
is being built or not. `module.nix` contains metadata about machines from which
confctl knows how to treat them. It is also used to declare which input roles
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

    # Channels come from mkConfctlOutputs
    inputs.channels = [ "nixos-unstable" ];

    # If the machine name is not a hostname, configure the address to which
    # should confctl deploy it
    host.target = "<ip address>";
  };
}
```

See [man/man8/confctl-options.nix.8.md](./man/man8/confctl-options.nix.8.md)
for a list of all options.

## Per-machine input overrides

Define the source as a root flake input, then map the desired role to it:

```nix
# In flake.nix:
inputs.nixpkgsCustom.url = "github:NixOS/nixpkgs/my-branch";

# In cluster/my-machine/module.nix:
cluster."my-machine".inputs.overrides.nixpkgs = "nixpkgsCustom";
```

Overrides can also add roles that are absent from the selected channels.

## Extra module arguments
Machine configs can use the following extra module arguments:

- `confDir` - path to the cluster configuration directory
- `confLib` - confctl functions, see [nix/lib/default.nix](nix/lib/default.nix)
- `confData` - access to user-defined datasets found in `data/default.nix`,
  see [example/data/default.nix](example/data/default.nix)
- `confMachine` - attrset with information about the machine that is currently
  being built, contains key `name` and all options from
  [machine metadata module](#machine-metadata-and-inputs)
- `flakeInputs` - flake inputs passed to `mkConfctlOutputs` (excluding `self`)
- `configurationInfo` - exact source revision and dirty state of the
  configuration flake when Git metadata is available, exposed as
  `confctl.configurationInfo` and written to
  `/etc/confctl/configuration-info.json`
- `inputs` - attrset of selected source store paths keyed by dependency role
- `inputsInfo` - metadata about flake inputs selected for the machine (keys are
  roles like `nixpkgs`/`vpsadminos`, values include `input`, `url`, `rev`,
  `shortRev`, `lastModified` when available), exposed as `confctl.inputsInfo` and written to
  `/etc/confctl/inputs-info.json`

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
    list.columns = [
      "name"
      "spin"
      "host.fqdn"
    ];
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

## Local generation formats

Local generation records must explicitly contain `mode: "flakes"`. Records in
other formats, records without a mode and corrupt records are excluded with their
paths and reasons reported. Listing and retention count supported generations;
excluded records and their GC roots remain untouched.

If `current` points to an excluded or missing record, selecting `current`, local
`old` removal and automatic local rotation fail. Numeric local selection also
fails when any records are excluded; use an explicit supported generation name.
A successful new build can deliberately update `current`. Remote profile
operations remain available, including profiles built with older tools.

Retain confctl v3 to use old software-pin generations. See the
[upgrade and rollback guide](docs/swpins-to-flakes.md) before removing any old
records or roots.

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

## Migrating Ruby extensions

confctl no longer loads `ConfCtl::UserScript` classes, custom Ruby commands or
Ruby hooks from `scripts/`. The following typed settings and commands cover
runtime-kernel collection and netboot inventory generation:

* [`runtime-kernels update`](man/man8/confctl-reference.8.md) maintains the configured
  kernel state. `confctl.runtimeKernels.refreshBeforeDeploy` refreshes that
  state before a new deployment build.
* [`netbootInventory`](docs/configuration-commands.md) publishes the configured
  netboot machine list after add, rename and rediscover.

Pair the tool/module pin, settings and removal of the corresponding old script.
Preserve existing state and generated files. Configuration edits reject remaining
legacy scripts; move independent tools outside the confctl extension directory.
For other custom behavior, migrate the caller explicitly; there is no Ruby
extension loader in the CLI. Tools-mode gems remain available to separate
configuration-repository programs. Rollback requires the matched old pin,
settings and scripts, as described in the configuration guide.

## More information
See the [man pages](./man/man8) and focused guides:

* [confctl(8)](./man/man8/confctl.8.md)
* [Rust CLI reference](./man/man8/confctl-reference.8.md)
* [kexec-netboot(8)](./man/man8/kexec-netboot.8.md)
* [confctl-options.nix(8)](./man/man8/confctl-options.nix.8.md)
* [Migrating from swpins to flakes](docs/swpins-to-flakes.md)
* [CLI and state compatibility during the Rust replacement](docs/compatibility.md)
* [Terminal and output ownership in the Rust implementation](docs/terminal-and-output.md)
