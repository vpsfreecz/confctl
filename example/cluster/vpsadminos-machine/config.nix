{ config, pkgs, lib, ... }:
{
  imports = [
    ../../environments/base.nix
    ./hardware.nix
  ];

  networking.hostName = "vpsadminos-machine";

  boot.loader.grub.enable = true;
  boot.loader.grub.version = 2;
  # boot.loader.grub.device = "";

  boot.supportedFilesystems = [ "zfs" ];
  boot.kernelParams = [ "nolive" ];

  boot.zfs.pools = {
    # ZFS pool configuration
  };

  system.stateVersion = "20.09";
}
