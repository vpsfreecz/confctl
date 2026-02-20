{ config, ... }:
{
  cluster."vpsadminos-machine" = {
    spin = "nixos";
    pins.channels = [
      "vpsadminos-staging"
      "nixos-unstable"
    ];
    host.target = "<ip address>";
  };
}
