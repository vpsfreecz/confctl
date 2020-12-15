{ config, pkgs, lib, ... }:
{
  imports = [
    ../../environments/base.nix
    <vpsadminos/os/lib/nixos-container/vpsadminos.nix>
  ];

  networking.hostName = "vpsfreecz-vps";

  environment.systemPackages = with pkgs; [
    vim
  ];

  networking.useDHCP = false;
  services.resolved.enable = false;
  systemd.extraConfig = ''
    DefaultTimeoutStartSec=900s
  '';
  systemd.services.systemd-udev-trigger.enable = false;

  documentation.enable = true;
  documentation.nixos.enable = true;

  system.stateVersion = "20.09";
}
