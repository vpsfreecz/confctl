{ config, ... }:
{
  cluster."vpsadminos-container" = {
    spin = "nixos";
    swpins.channels = [ "nixos-unstable" "vpsadminos-staging" ];
    host.target = "<ip address>";
  };
}
