{ config, ... }:
{
  cluster."nixos-machine" = {
    spin = "nixos";
    pins.channels = [ "nixos" ];
    host.target = "localhost";
  };
}
