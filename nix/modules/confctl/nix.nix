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
            If true, confctl passes --impure to nix eval/nix build (flake backend).
            Needed when evaluation/build references host paths like /secrets.
          '';
        };
      };
    };
  };
}
