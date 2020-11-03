{ config, ... }:
{
  cluster."vpsadminos-container" = {
    spin = "nixos";
    host.target = "<ip address>";
  };
}
