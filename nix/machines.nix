{ confDir, corePkgs, coreLib }:
let
  confLib = import ./lib { inherit confDir coreLib corePkgs; };

  userModules = "${toString confDir}/modules/cluster/default.nix";

  baseModules = [
    ({ ... }:
    {
      _module.args = {
        pkgs = corePkgs;
        inherit confLib;
        swpins = {};
        swpinsInfo = {};
        confMachine = null;
      };
    })

    ./modules/cluster
  ] ++ (import "${toString confDir}/cluster/module-list.nix")
    ++ (coreLib.optional (builtins.pathExists userModules) userModules);

  evalConfig = corePkgs.lib.evalModules {
    prefix = [];
    check = true;
    modules = baseModules;
  };

  cluster = evalConfig.config.cluster;

  allMachines = confLib.getClusterMachines cluster;
in allMachines
