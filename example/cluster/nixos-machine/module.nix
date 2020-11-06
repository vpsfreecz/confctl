{ config, ... }:
{
  cluster."nixos-machine" = {
    spin = "nixos";
    swpins.channels = [ "nixos-unstable" ];
    host.target = "<ip address>";
  };
}
