{ config, pkgs, lib, swpinsInfo, ... }:
with lib;
let
  swpinOptions = import ../../lib/swpins/options.nix { inherit lib; };

  machineSwpinsInfo = pkgs.writeText "swpins-info.json" (builtins.toJSON swpinsInfo);
in {
  options = {
    confctl = {
      swpins.core = {
        pins = mkOption {
          type = types.attrsOf (types.submodule swpinOptions.specModule);
          default = {
            nixpkgs = {
              type = "git-rev";
              git-rev = {
                url = "https://github.com/NixOS/nixpkgs";
                update.ref = "refs/heads/nixos-unstable";
                update.auto = true;
                update.interval = 30*24*60*60; # 1 month
              };
            };
          };
          description = ''
            Core software packages used internally by confctl

            It has to contain package <literal>nixpkgs</literal>, which is used
            to resolve other software pins from channels or cluster machines.
          '';
        };

        channels = mkOption {
          type = types.listOf types.str;
          default = [];
          description = ''
            List of channels from <option>confctl.swpins.channels</option>
            to use for core swpins
          '';
        };
      };

      swpins.channels = mkOption {
        type = types.attrsOf (types.attrsOf (types.submodule swpinOptions.specModule));
        default = {};
        description = ''
          Software pin channels
        '';
      };
    };
  };

  config = {
    environment.etc."confctl/swpins-info.json".source = machineSwpinsInfo;
  };
}
