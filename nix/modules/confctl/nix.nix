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
            Maximum number of build jobs, passed to <literal>nix build</literal>
            commands.
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
            builds to support legacy <literal>&lt;nixpkgs&gt;</literal> or <literal>&lt;vpsadminos&gt;</literal> imports.
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
            List of input roles that should be mapped to NIX_PATH when
            legacyNixPath is enabled.
          '';
        };
      };
    };
  };
}
