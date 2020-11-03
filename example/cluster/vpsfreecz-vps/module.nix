{ config, ... }:
{
  cluster."vpsfreecz-vps" = {
    spin = "nixos";
    host.target = "<ip address>";
  };
}
