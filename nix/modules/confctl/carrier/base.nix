{
  config,
  pkgs,
  lib,
  confMachine,
  ...
}:
let
  inherit (lib) mkIf mkOption types;

  cfg = config.confctl.carrier;

  rustPackage = config._module.args.confctlRustPackage or null;
  onChangeCommands = pkgs.writeScript "carrier-on-change-commands.sh" ''
    #!${pkgs.bash}/bin/bash
    ${cfg.onChangeCommands}
  '';

  carrier-env =
    if rustPackage != null then
      pkgs.writeShellScriptBin "carrier-env" ''
        export PATH=${lib.escapeShellArg (lib.makeBinPath [ pkgs.nix ])}:"$PATH"
        export CONFCTL_INTERNAL_CARRIER_ON_CHANGE=${lib.escapeShellArg (toString onChangeCommands)}
        exec -a "$0" ${rustPackage}/libexec/confctl/carrier-env "$@"
      ''
    else
      pkgs.replaceVarsWith {
        src = ./carrier-env.rb;
        name = "carrier-env";
        isExecutable = true;
        dir = "bin";
        replacements = {
          ruby = pkgs.ruby;
          inherit onChangeCommands;
        };
      };
in
{
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
