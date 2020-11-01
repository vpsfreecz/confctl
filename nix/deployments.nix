{ confDir, pkgs, lib }:
let
  confLib = import ./lib { inherit confDir lib pkgs; };

  baseModules = [
    ./modules/cluster
  ] ++ (import "${toString confDir}/cluster/module-list.nix");

  evalConfig = pkgs.lib.evalModules {
    prefix = [];
    check = true;
    modules = baseModules;
    args = {};
  };

  cluster = evalConfig.config.cluster;

  allDeployments = confLib.getClusterDeployments cluster;
in allDeployments
