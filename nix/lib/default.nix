{ confDir, coreLib, corePkgs }:
with coreLib;
let
  machine = import ./machine { inherit confDir corePkgs coreLib findConfig; };

  findConfig =
    { cluster, name }:
    cluster.${name};

  makeMachine =
    { name, config }:
    {
      inherit name config;
      build.toplevel = buildConfig { inherit name config; };
    };

  buildConfig =
    { name, config }:
    if !config.managed then
      null
    else if config.spin == "nixos" then
      machine.nixos { inherit name config; }
    else if config.spin == "vpsadminos" then
      machine.vpsadminos { inherit name config; }
    else
      null;
in rec {
  inherit corePkgs coreLib;

  mkNetUdevRule = name: mac: ''
  ACTION=="add", SUBSYSTEM=="net", DRIVERS=="?*", KERNEL=="eth*", ATTR{address}=="${mac}", NAME="${name}"
  '';

  mkNetUdevRules = rs: concatStringsSep "\n" (mapAttrsToList (name: mac:
    mkNetUdevRule name mac
  ) rs);

  inherit findConfig;

  # Return all configured machines in a list
  getClusterDeployments = cluster:
    mapAttrsToList (name: config:
      makeMachine { inherit name config; }
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
