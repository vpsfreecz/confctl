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
