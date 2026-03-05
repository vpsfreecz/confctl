{ config, ... }:
{
  cluster."vpsadminos-machine" = {
    spin = "vpsadminos";
    swpins.channels = [
      "vpsadminos-staging"
      "nixos-unstable"
    ];
    host.target = "<ip address>";
  };
}
