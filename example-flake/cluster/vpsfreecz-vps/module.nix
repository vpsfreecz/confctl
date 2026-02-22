{ config, ... }:
{
  cluster."vpsfreecz-vps" = {
    spin = "nixos";
    inputs.channels = [ "vpsadminos" ];
    host.target = "<ip address>";
  };
}
