{ config, ... }:
{
  cluster."nested/nixos-machine" = {
    spin = "nixos";
    pins.channels = [ "nixos" ];
    host.target = "localhost";
  };
}
