{ lib }:
with lib;
rec {
  specModule =
    { config, ... }:
    {
      options = {
        type = mkOption {
          type = types.enum [ "git" "git-rev" ];
          default = "git";
        };

        git = mkOption {
          type = types.nullOr (types.submodule gitSpec);
          default = null;
        };

        git-rev = mkOption {
          type = types.nullOr (types.submodule gitSpec);
          default = null;
        };
      };
    };

  gitSpec =
    { config, ... }:
    {
      options = {
        url = mkOption {
          type = types.str;
          description = ''
            URL of the git repository
          '';
          example = "https://github.com/vpsfreecz/vpsadminos";
        };

        fetchSubmodules = mkOption {
          type = types.bool;
          default = false;
          description = ''
            Fetch git submodules
          '';
        };

        update = mkOption {
          type = types.nullOr types.str;
          default = null;
          description = ''
            Implicit git reference to use for automatic updates
          '';
          example = "refs/heads/master";
        };
      };
    };
}
