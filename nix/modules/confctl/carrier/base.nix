{ config, pkgs, lib, confMachine, ... }:
let
  inherit (lib) mkIf mkOption types;

  cfg = config.confctl.carrier;

  carrier-env = pkgs.confReplaceVarsWith {
    src = ./carrier-env.rb;
    name = "carrier-env";
    isExecutable = true;
    dir = "bin";
    replacements = {
      ruby = pkgs.ruby;
      onChangeCommands = pkgs.writeScript "carrier-on-change-commands.sh" ''
        #!${pkgs.bash}/bin/bash
        ${cfg.onChangeCommands}
      '';
    };
  };
in {
  options = {
    confctl.carrier.onChangeCommands = mkOption {
      type = types.lines;
      default = "";
      description = ''
        Extra commands executed on a carrier machine when carried machine
        is deployed or removed
      '';
    };
  };

  config = mkIf confMachine.carrier.enable {
    environment.systemPackages = [
      carrier-env
    ];
  };
}