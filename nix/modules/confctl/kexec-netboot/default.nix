{
  config,
  lib,
  pkgs,
  confMachine,
  ...
}:
let
  inherit (lib) mkEnableOption mkIf;

  cfg = config.confctl.programs.kexec-netboot;

  manPage = import ../../../kexec-netboot-man.nix { inherit pkgs; };

  kexecNetbootBin = import ../../../helpers/kexec-netboot.nix {
    inherit pkgs manPage;
    machineFqdn = confMachine.host.fqdn;
  };
in
{
  options = {
    confctl.programs.kexec-netboot = {
      enable = mkEnableOption "Enable kexec-netboot utility";
    };
  };

  config = mkIf cfg.enable {
    environment.systemPackages = [ kexecNetbootBin ];
  };
}
