{ config, ... }:
{
  cluster."vpsadminos-container" = {
    spin = "nixos";
    pins.channels = [ "vpsadminos" ];
    host.target = "<ip address>";
  };
}
