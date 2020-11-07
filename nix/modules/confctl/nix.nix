{ config, lib, ... }:
with lib;
{
  options = {
    confctl = {
      nix = {
        nixPath = mkOption {
          type = types.listOf types.str;
          default = [];
          description = ''
            List of extra paths added to environment variable
            <literal>NIX_PATH</literal> for all <literal>nix-build</literal>
            invokations
          '';
        };
      };
    };
  };
}
