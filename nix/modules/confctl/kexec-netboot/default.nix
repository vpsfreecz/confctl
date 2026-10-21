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

  rustPackage =
    config._module.args.confctlRustPackage or (import ../../../rust-package.nix { inherit pkgs; });

  kexecNetboot = pkgs.writeShellScript "kexec-netboot" ''
    if [ -z "''${SSL_CERT_FILE+x}" ]; then
      export SSL_CERT_FILE=${lib.escapeShellArg "${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"}
    fi
    if [ -z "''${NIX_SSL_CERT_FILE+x}" ]; then
      export NIX_SSL_CERT_FILE=${lib.escapeShellArg "${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"}
    fi
    export CONFCTL_INTERNAL_KEXEC_MACHINE_FQDN=${lib.escapeShellArg confMachine.host.fqdn}
    export CONFCTL_INTERNAL_KEXEC_EXECUTABLE=${lib.escapeShellArg "${pkgs.kexec-tools}/bin/kexec"}
    exec -a "$0" ${rustPackage}/libexec/confctl/kexec-netboot "$@"
  '';

  kexecNetbootBin = pkgs.runCommand "kexec-netboot-bin" { } ''
    mkdir -p $out/bin
    ln -s ${kexecNetboot} $out/bin/kexec-netboot

    mkdir -p $out/share/man/man8
    cp ${rustPackage}/share/man/man8/kexec-netboot.8 $out/share/man/man8/kexec-netboot.8
  '';
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
