{ config, lib, ... }:
with lib;
let
  swpinOptions = import ../../lib/swpins/options.nix { inherit lib; };
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
}
