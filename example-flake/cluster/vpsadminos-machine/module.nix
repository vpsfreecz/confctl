{ config, ... }:
{
  cluster."vpsadminos-machine" = {
    spin = "nixos";
    inputs.channels = [ "vpsadminos" ];
    host.target = "<ip address>";
  };
}
