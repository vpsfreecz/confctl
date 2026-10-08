{ config, ... }:
{
  cluster."vpsadminos-container" = {
    spin = "nixos";
    inputs.channels = [ "vpsadminos" ];
    host.target = "<ip address>";
  };
}
