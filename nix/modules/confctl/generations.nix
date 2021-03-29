{ config, lib, ... }:
with lib;
{
  options = {
    confctl = {
      buildGenerations = {
        min = mkOption {
          type = types.int;
          default = 4;
          description = ''
            The minimum number of build generations to be kept.

            This is the default value, which can be overriden per host.
          '';
        };

        max = mkOption {
          type = types.int;
          default = 30;
          description = ''
            The maximum number of build generations to be kept.

            This is the default value, which can be overriden per host.
          '';
        };

        maxAge = mkOption {
          type = types.int;
          default = 90*24*60*60;
          description = ''
            Delete build generations older than
            <option>confctl.buildGenerations.maxAge</option> seconds. Old generations
            are deleted even if <option>confctl.buildGenerations.max</option> is
            not reached.

            This is the default value, which can be overriden per host.
          '';
        };
      };

      hostGenerations = {
        min = mkOption {
          type = types.int;
          default = 4;
          description = ''
            The minimum number of generations to be kept on deployed hosts.

            This is the default value, which can be overriden per host.
          '';
        };

        max = mkOption {
          type = types.int;
          default = 30;
          description = ''
            The maximum number of generations to be kept on deployed hosts.

            This is the default value, which can be overriden per host.
          '';
        };

        maxAge = mkOption {
          type = types.int;
          default = 90*24*60*60;
          description = ''
            Delete generations older than
            <option>confctl.hostGenerations.maxAge</option> seconds from
            deployed hosts. Old generations
            are deleted even if <option>confctl.hostGenerations.max</option> is
            not reached.

            This is the default value, which can be overriden per host.
          '';
        };
      };
    };
  };
}
