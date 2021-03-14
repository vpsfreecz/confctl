{ config, ... }:
{
  cluster."vpsadminos-container" = {
    spin = "nixos";
    swpins.channels = [ "nixos-unstable" "vpsadminos-master" ];
    host.target = "<ip address>";
  };
}
