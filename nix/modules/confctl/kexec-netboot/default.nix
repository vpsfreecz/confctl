{ config, lib, pkgs, confMachine, ... }:
let
  inherit (lib) mkEnableOption mkIf;

  cfg = config.confctl.programs.kexec-netboot;

  kexecNetboot = pkgs.substituteAll {
    name = "kexec-netboot";
    src = ./kexec-netboot.rb;
    isExecutable = true;
    ruby = pkgs.ruby;
    kexecTools = pkgs.kexec-tools;
    machineFqdn = confMachine.host.fqdn;
  };

  kexecNetbootBin = pkgs.runCommand "kexec-netboot-bin" {} ''
    mkdir -p $out/bin
    ln -s ${kexecNetboot} $out/bin/kexec-netboot

    mkdir -p $out/share/man/man8
    ${pkgs.asciidoctor}/bin/asciidoctor \
      -b manpage \
      -D $out/share/man/man8 \
      ${./kexec-netboot.8.adoc}
  '';
in {
  options  = {
    confctl.programs.kexec-netboot = {
      enable = mkEnableOption "Enable kexec-netboot utility";
    };
  };

  config = mkIf cfg.enable {
    environment.systemPackages = [ kexecNetbootBin ];
  };
}