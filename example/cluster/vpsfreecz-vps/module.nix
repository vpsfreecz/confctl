{ config, ... }:
{
  cluster."vpsfreecz-vps" = {
    spin = "nixos";
    swpins.channels = [ "nixos-unstable" ];
    host.target = "<ip address>";
  };
}
