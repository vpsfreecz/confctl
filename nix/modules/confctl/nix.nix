{ config, lib, ... }:
with lib;
{
  options = {
    confctl = {
      nix = {
        maxJobs = mkOption {
          type = types.nullOr (types.either types.int (types.enum [ "auto" ]));
          default = null;
          description = ''
            Maximum number of build jobs, passed to <literal>nix-build</literal>
            commands.
          '';
        };

        nixPath = mkOption {
          type = types.listOf types.str;
          default = [ ];
          description = ''
            List of extra paths added to environment variable
            <literal>NIX_PATH</literal> for all <literal>nix-build</literal>
            invokations
          '';
        };

        impureEval = mkOption {
          type = types.bool;
          default = false;
          description = ''
            Enable impure evaluation/builds (allows reading host paths outside the Nix store when they are referenced as Nix paths).
          '';
        };

        legacyNixPath = mkOption {
          type = types.bool;
          default = false;
          description = ''
            If true, confctl adds -I mappings for selected inputs during flake
            builds to support legacy <nixpkgs> or <vpsadminos> imports.
          '';
        };

        legacyNixPathMap = mkOption {
          type = types.listOf types.str;
          default = [
            "nixpkgs"
            "vpsadminos"
            "vpsadmin"
          ];
          description = ''
            List of input names that should be mapped to NIX_PATH when
            legacyNixPath is enabled.
          '';
        };
      };
    };
  };
}
