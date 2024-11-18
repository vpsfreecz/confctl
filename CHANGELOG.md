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
