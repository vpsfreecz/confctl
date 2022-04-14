{ config, ... }:
{
  cluster."vpsadminos-machine" = {
    spin = "nixos";
    swpins.channels = [ "vpsadminos-staging" "nixos-unstable" ];
    host.target = "<ip address>";
  };
}
