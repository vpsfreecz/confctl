# Unreleased

- Remove software-pin configuration, commands, migration helper and pin metadata.
  Cluster configurations now require `flake.nix`. Migrate with confctl v3 before
  upgrading; keep v3 and required generations/GC roots for rollback.
- Exclude unsupported local generation records with explicit diagnostics and
  safeguards for current, numeric selection and retention. Existing flake JSON
  and GC-root names are unchanged; excluded state is preserved.
- Generate the option reference and custom metadata listing from flake outputs,
  including carrier and program options. Use the flake configuration in `example/`.

# Wed Oct 07 2026 -- version 3.0.0

Software pins (`swpins`) remain supported in v3.0.0 and will be removed in v4.
Use [the migration guide](docs/swpins-to-flakes.md) to move existing
configurations to flake inputs.

## Flake configurations

- Add a flake backend for building and deploying NixOS and vpsAdminOS machines
- Create flake-based configurations by default with `confctl init`; use
  `confctl init --swpins` for the legacy software pin workflow
- Add `confctl inputs` commands to list, update and set input revisions at the
  root, channel and machine levels, with optional commits and changelogs
- Map dependency roles to flake inputs through channels and per-machine overrides;
  see [docs/flake-inputs.md](docs/flake-inputs.md)
- Add `confctl migrate swpins-to-flakes`, including `--dry-run` and individual
  migration steps
- Record flake inputs in build generations and show their revisions in generation
  listings, status, diffs and changelogs
- Record the configuration source revision in deployed machine metadata
- Add flake packages, configuration development shells and an example flake
  configuration
- Support optional impure evaluation and legacy `NIX_PATH` mappings for flake
  builds
- Evaluate only requested flake build outputs and build machine metadata as JSON
- Group shared input changelogs and report resolved revisions after setting inputs
- Fail input updates when Nix falls back to cached GitHub metadata

## Deployment and fixes

- Skip deployment to machines already using the target generation, avoiding
  unnecessary copying, activation, reboots and health checks
- Support custom SSH ports through `host.port`, including Nix copies
- Preserve spaces and shell metacharacters in remote command arguments
- Fix deployment confirmation on localhost
- Support CLI output and progress reporting without a terminal
- Reuse prefetched Git checkouts when updating software pins and fix commits of
  changed software pin files
- Fix garbage collection roots for software pin paths
- Support NixOS in `kexec-netboot`, stream downloads and add `--no-sync`
- Honor NixOS PXE kernel parameters and preserve the init path for carried images
- Add RSpec coverage and integration tests for flakes, software pins, automatic
  rollback, carriers and netboot
- Refresh Ruby dependencies, including the vpsAdmin and HaveAPI clients

## Upgrade notes

- Require Ruby 3.3 or newer (previously Ruby 3.1)
- Remove the `confReplaceVarsWith` compatibility wrapper; confctl's Nix modules
  now require `pkgs.replaceVarsWith`
- Existing software pin configurations and generations remain supported. Migrating
  to flakes is optional in v3.0.0. Keep the migration on a configuration branch;
  confctl v2 cannot use the resulting flake configuration.

# Fri Jun 06 2025 -- version 2.2.3
- Open git commit editor only when running in a terminal

# Fri Jun 06 2025 -- version 2.2.2
- Fix `--[no-]editor` option on `confctl swpins cluster/channel update` commands

# Fri Jun 06 2025 -- version 2.2.1
- Fix `--[no-]editor` option on `confctl swpins core` commands

# Fri Jun 06 2025 -- version 2.2.0
- Handle `pkgs.substituteAll` / `pkgs.replaceVarsWith` compatibility on NixOS unstable
  and 25.05
- Change swpin files only when revisions are updated, skip commit when no changes were made
- Add option `--[no-]editor` to `confctl swpins core/cluster/channel set/update` commands

# Sun May 11 2025 -- version 2.1.0
- Support for referring to generations by their offset
- Resolved generations are printed on build/deploy/etc.
- Automatically rollback faulty configurations
- Interleave copying of carried machine generations
- Add `confctl.programs.kexec-netboot`
- Let the user retry dry activation in interactive mode
- Fix listing, deletion and garbage collection of carried machines' generations
- Read and display kernel version for each generation
- Option to disable the garbage collection in `confctl generation rotate`
- Shorten titles and entries in netboot menus

# Sun Nov 17 2024 -- version 2.0.0
- Distinguish machine `config` and `metaConfig` (breaking change)
- Support for machine carriers and netboot servers
- Optimized git fetch calls when updating software pins
- Added option `--cores` that is passed to nix-build
- Added RuboCop
- Bug fixes

## Transition to `metaConfig`
The use of `config` has been ambiguous, it could either mean machine
configuration, i.e. the result of all configured NixOS/vpsAdminOS options,
or it could mean machine metadata from `module.nix`. Machine metadata
is now accessible as `metaConfig`.

- `confLib.findConfig` has been renamed to `confLib.findMetaConfig`
- `confLib.getClusterMachines` returns a list of machines with `metaConfig` attribute

# Sat Feb 17 2024 -- version 1.0.0
- Initial release
