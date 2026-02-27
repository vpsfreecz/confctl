{ lib, confMachine, ... }:
let
  host = confMachine.host;
in
{
  networking.hostName = lib.mkIf (host != null && host ? name && host.name != null) (
    lib.mkDefault host.name
  );

  networking.domain = lib.mkIf (host != null && host ? fullDomain && host.fullDomain != null) (
    lib.mkDefault host.fullDomain
  );
}
