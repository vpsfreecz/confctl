{ config, pkgs, lib, swpinsInfo, ... }:
with lib;
let
  swpinOptions = import ../../lib/swpins/options.nix { inherit lib; };

  deploymentSwpinsInfo = pkgs.writeText "swpins-info.json" (builtins.toJSON swpinsInfo);
in {
  options = {
    confctl = {
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
    environment.etc."confctl/swpins-info.json".source = deploymentSwpinsInfo;
  };
}
