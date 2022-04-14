{ config, ... }:
{
  cluster."vpsfreecz-vps" = {
    spin = "nixos";
    swpins.channels = [ "nixos-unstable" "vpsadminos-staging" ];
    host.target = "<ip address>";
  };
}
