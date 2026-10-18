{ lib, ... }:
let
  inherit (lib) mkOption types;
in
{
  options.confctl.netbootInventory = {
    enable = mkOption {
      type = types.bool;
      default = false;
      description = "Regenerate the netboot machine list after add, rename or rediscover publishes the core cluster list.";
    };
    outputFile = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Existing-parent, configuration-relative output for the generated Nix list. Required when enabled.";
    };
  };
}
