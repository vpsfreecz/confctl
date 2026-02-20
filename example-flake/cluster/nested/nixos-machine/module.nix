{ config, ... }:
{
  cluster."nested/nixos-machine" = {
    spin = "nixos";
    pins.channels = [ "nixos-unstable" ];
    host.target = "localhost";
  };
}
