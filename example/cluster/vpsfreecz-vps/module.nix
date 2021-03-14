{ config, ... }:
{
  cluster."vpsfreecz-vps" = {
    spin = "nixos";
    swpins.channels = [ "nixos-unstable" "vpsadminos-master" ];
    host.target = "<ip address>";
  };
}
