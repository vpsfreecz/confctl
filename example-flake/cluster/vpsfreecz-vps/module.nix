{ config, ... }:
{
  cluster."vpsfreecz-vps" = {
    spin = "nixos";
    pins.channels = [
      "nixos-unstable"
      "vpsadminos-staging"
    ];
    host.target = "<ip address>";
  };
}
