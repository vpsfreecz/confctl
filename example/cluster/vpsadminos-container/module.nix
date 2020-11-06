{ config, ... }:
{
  cluster."vpsadminos-container" = {
    spin = "nixos";
    swpins.channels = [ "nixos-unstable" ];
    host.target = "<ip address>";
  };
}
