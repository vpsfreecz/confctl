{ confDir, lib, pkgs }:
with lib;
let
  deployment = import ./deployment { inherit confDir pkgs lib findConfig; };

  findConfig =
    { cluster, name }:
    cluster.${name};

  makeDeployment =
    { name, config }:
    {
      inherit name config;
      build.toplevel = buildConfig {
        inherit name;
        inherit (config) managed spin;
      };
    };

  buildConfig =
    { name, managed, spin, ... }:
    if !managed then
      null
    else if spin == "nixos" then
      deployment.nixos { inherit name; }
    else if spin == "vpsadminos" then
      deployment.vpsadminos { inherit name; }
    else
      null;
in rec {
  mkNetUdevRule = name: mac: ''
  ACTION=="add", SUBSYSTEM=="net", DRIVERS=="?*", KERNEL=="eth*", ATTR{address}=="${mac}", NAME="${name}"
  '';

  mkNetUdevRules = rs: concatStringsSep "\n" (mapAttrsToList (name: mac:
    mkNetUdevRule name mac
  ) rs);

  inherit findConfig;

  # Return all configured deployments in a list
  getClusterDeployments = cluster:
    mapAttrsToList (name: config:
      makeDeployment { inherit name config; }
    ) cluster;

  # Get IP version addresses from all machines in a cluster
  getAllAddressesOf = cluster: v:
    let
      deps = getClusterDeployments cluster;
      addresses = flatten (map (d:
        map (a: d // a) d.config.addresses.${"v${toString v}"}
      ) deps);
    in addresses;
}
