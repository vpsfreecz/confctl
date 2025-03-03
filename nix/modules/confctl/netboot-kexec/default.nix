{ config, lib, pkgs, confMachine, ... }:
let
  inherit (lib) mkEnableOption mkIf;

  cfg = config.confctl.programs.netboot-kexec;

  netbootKexec = pkgs.substituteAll {
    name = "netboot-kexec";
    src = ./netboot-kexec.rb;
    isExecutable = true;
    ruby = pkgs.ruby;
    kexecTools = pkgs.kexec-tools;
    machineFqdn = confMachine.host.fqdn;
  };

  netbootKexecBin = pkgs.runCommand "netboot-kexec-bin" {} ''
    mkdir -p $out/bin
    ln -s ${netbootKexec} $out/bin/netboot-kexec

    mkdir -p $out/share/man/man8
    ${pkgs.asciidoctor}/bin/asciidoctor \
      -b manpage \
      -D $out/share/man/man8 \
      ${./netboot-kexec.8.adoc}
  '';
in {
  options  = {
    confctl.programs.netboot-kexec = {
      enable = mkEnableOption "Enable netboot-kexec utility";
    };
  };

  config = mkIf cfg.enable {
    environment.systemPackages = [ netbootKexecBin ];
  };
}