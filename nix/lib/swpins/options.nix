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

        update = {
          ref = mkOption {
            type = types.nullOr types.str;
            default = null;
            description = ''
              Implicit git reference to use for both manual and automatic updates
            '';
            example = "refs/heads/master";
          };

          auto = mkOption {
            type = types.bool;
            default = false;
            description = ''
              When enabled, the pin is automatically updated to
              <literal>ref</literal> before building machines.
            '';
          };

          interval = mkOption {
            type = types.int;
            default = 60*60;
            description = ''
              Number of seconds from the last update to trigger the next
              auto-update, if auto-update is enabled.
            '';
          };
        };
      };
    };
}
