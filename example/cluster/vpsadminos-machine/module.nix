{ config, ... }:
{
  cluster."vpsadminos-machine" = {
    spin = "nixos";
    host.target = "<ip address>";
  };
}
