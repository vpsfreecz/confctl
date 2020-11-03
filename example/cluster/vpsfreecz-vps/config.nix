{ config, pkgs, lib, ... }:
{
  imports = [
    ../../environments/base.nix
    <nixpkgs/nixos/modules/virtualisation/container-config.nix>
    <vpsadminos/os/lib/nixos-container/build.nix>
    <vpsadminos/os/lib/nixos-container/networking.nix>
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
