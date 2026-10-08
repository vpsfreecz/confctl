{ config, ... }:
{
  cluster."nixos-machine" = {
    spin = "nixos";
    inputs.channels = [ "nixos" ];
    host.target = "localhost";
  };
}
