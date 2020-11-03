{ config, pkgs, lib, ... }:
{
  imports = [
    ../../environments/base.nix
    ./hardware.nix
  ];

  networking.hostName = "nixos-machine";

  boot.loader.grub.enable = true;
  boot.loader.grub.version = 2;
  # boot.loader.grub.device = "";

  system.stateVersion = "20.09";
}
