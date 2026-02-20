{ config, ... }:
{
  cluster."vpsadminos-container" = {
    spin = "nixos";
    pins.channels = [
      "nixos-unstable"
      "vpsadminos-staging"
    ];
    host.target = "<ip address>";
  };
}
