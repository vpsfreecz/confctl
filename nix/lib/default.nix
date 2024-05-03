{ confDir, coreLib, corePkgs }:
with coreLib;
let
  machine = import ./machine { inherit confDir corePkgs coreLib findMetaConfig; };

  findMetaConfig =
    { cluster, name }:
    cluster.${name};

  makeMachine =
    { name, metaConfig, carrier ? null, alias ? null, clusterName ? null, extraModules ? [], buildAttribute ? null }:
    let
      ensuredClusterName = if isNull clusterName then name else clusterName;
    in {
      inherit name alias metaConfig carrier extraModules;
      clusterName = ensuredClusterName;

      build = {
        attribute = if isNull buildAttribute then metaConfig.buildAttribute else buildAttribute;
        toplevel = buildConfig { name = ensuredClusterName; inherit metaConfig; };
      };
    };

  buildConfig =
    { name, metaConfig }:
    if !metaConfig.managed then
      null
    else if metaConfig.spin == "nixos" then
      machine.nixos { inherit name metaConfig; }
    else if metaConfig.spin == "vpsadminos" then
      machine.vpsadminos { inherit name metaConfig; }
    else
      null;

  expandCarriers = machineAttrs: flatten (mapAttrsToList (name: m:
    if m.metaConfig.carrier.enable then
      [ m ] ++ (expandCarrier machineAttrs m)
    else
      m
  ) machineAttrs);

  expandCarrier = machineAttrs: carrierMachine: map (cm:
    makeMachine {
      name = "${carrierMachine.name}#${if isNull cm.alias then cm.machine else cm.alias}";
      alias = cm.alias;
      clusterName = cm.machine;
      carrier = carrierMachine.name;
      extraModules = cm.extraModules;
      buildAttribute = cm.buildAttribute;
      metaConfig = machineAttrs.${cm.machine}.metaConfig;
    }
  ) carrierMachine.metaConfig.carrier.machines;
in rec {
  inherit corePkgs coreLib;

  mkNetUdevRule = name: mac: ''
  ACTION=="add", SUBSYSTEM=="net", DRIVERS=="?*", KERNEL=="eth*", ATTR{address}=="${mac}", NAME="${name}"
  '';

  mkNetUdevRules = rs: concatStringsSep "\n" (mapAttrsToList (name: mac:
    mkNetUdevRule name mac
  ) rs);

  inherit findMetaConfig;

  # Return all configured machines in a list
  getClusterMachines = cluster:
    let
      machineAttrs = mapAttrs (name: metaConfig:
        makeMachine { inherit name metaConfig; }
      ) cluster;
    in expandCarriers machineAttrs;

  # Get IP version addresses from all machines in a cluster
  getAllAddressesOf = cluster: v:
    let
      machines = getClusterMachines cluster;
      addresses = flatten (map (machine:
        map (addr: machine // addr) machine.metaConfig.addresses.${"v${toString v}"}
      ) machines);
    in addresses;

  mkOptions = {
    addresses = v:
      { config, ... }:
      {
        options = {
          address = mkOption {
            type = types.str;
            description = "IPv${toString v} address";
          };

          prefix = mkOption {
            type = types.ints.positive;
            description = "Prefix length";
          };

          string = mkOption {
            type = types.nullOr types.str;
            default = null;
            apply = v:
              if isNull v then
                "${config.address}/${toString config.prefix}"
              else
                v;
            description = "Address with prefix as string";
          };
        };
      };
  };
}
