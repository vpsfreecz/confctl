{ config, ... }:
{
  cluster."nixos-machine" = {
    spin = "nixos";
    host.target = "<ip address>";
  };
}
