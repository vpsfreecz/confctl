{ config, ... }:
{
  cluster."vpsadminos-machine" = {
    spin = "nixos";
    pins.channels = [ "vpsadminos" ];
    host.target = "<ip address>";
  };
}
