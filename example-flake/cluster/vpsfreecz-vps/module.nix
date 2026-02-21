{ config, ... }:
{
  cluster."vpsfreecz-vps" = {
    spin = "nixos";
    pins.channels = [ "vpsadminos" ];
    host.target = "<ip address>";
  };
}
