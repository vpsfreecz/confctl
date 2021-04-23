{ confDir, corePkgs, coreLib }:
let
  confLib = import ./lib { inherit confDir coreLib corePkgs; };

  baseModules = [
    ./modules/cluster
  ] ++ (import "${toString confDir}/cluster/module-list.nix");

  evalConfig = corePkgs.lib.evalModules {
    prefix = [];
    check = true;
    modules = baseModules;
    args = {};
  };

  cluster = evalConfig.config.cluster;

  allMachines = confLib.getClusterMachines cluster;
in allMachines
