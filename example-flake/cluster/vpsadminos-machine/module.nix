{ config, ... }:
{
  cluster."vpsadminos-machine" = {
    spin = "vpsadminos";
    inputs.channels = [ "vpsadminos" ];
    host.target = "<ip address>";
  };
}
