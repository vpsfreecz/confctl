{ config, ... }:
{
  cluster."vpsadminos-machine" = {
    spin = "nixos";
    swpins.channels = [ "vpsadminos-master" "nixos-unstable" ];
    host.target = "<ip address>";
  };
}
