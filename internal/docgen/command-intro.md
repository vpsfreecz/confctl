# NAME

`confctl` - Nix deployment configuration management tool

# SYNOPSIS

`confctl` [global options] command [command options] [arguments...]

# DESCRIPTION

`confctl` manages configurations for NixOS and vpsAdminOS machines. Its commands
cover configuration, input management, build, deployment and machine operations.
The command reference and help use the same CLI declarations.

# FLAKE INPUTS

Configurations provide `flake.nix` outputs through `confctl.lib.mkConfctlOutputs`
and keep dependency revisions in `flake.lock`. Channels map dependency roles to
root inputs. Machines select channels through `cluster.<name>.inputs.channels`;
later channels override earlier mappings, and `inputs.overrides` takes
precedence over channels. Build and deploy do not update locked inputs
automatically.

Software-pin configurations require migration with retained confctl v3 before
upgrading to the flake-only tool. The
[flake input guide](https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/docs/flake-inputs.md)
and [migration and rollback guide](https://github.com/vpsfreecz/confctl/blob/0b0e732d79ab32786b3da746c4754a8412c35374/docs/swpins-to-flakes.md)
describe the configuration and retained-closure requirements.
