{ lib, ... }:
let
  inherit (lib) mkOption types;
in
{
  options.confctl.runtimeKernels = {
    enable = mkOption {
      type = types.bool;
      default = false;
      description = "Enable the runtime kernel update command for this configuration.";
    };
    stateFile = mkOption {
      type = types.nullOr types.str;
      default = null;
      description = "Existing-parent, configuration-relative JSON state file. Required when enabled; mutable files stay in the live checkout.";
    };
    refreshBeforeDeploy = mkOption {
      type = types.bool;
      default = false;
      description = "Refresh non-carried vpsAdminOS machines from the confirmed deployment scope once before generation lookup or build evaluation. Requires enable.";
    };
  };
}
