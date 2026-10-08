{
  nixpkgs,
  confDir,
  system,
  flakeInputs ? { },
}:
let
  pkgs = import nixpkgs { inherit system; };
  lib = pkgs.lib;
  confLib = import ./lib {
    inherit confDir;
    corePkgs = pkgs;
    coreLib = lib;
  };
  userModules = confDir + "/modules/cluster/default.nix";
  evaluated = import (nixpkgs + "/nixos/lib/eval-config.nix") {
    inherit system;
    specialArgs = {
      inherit confLib flakeInputs;
      confMachine = {
        host = null;
        carrier.enable = false;
      };
      inputsInfo = null;
      configurationInfo = null;
    };
    modules =
      (import ./modules/module-list.nix).all
      ++ (import ./modules/system-list.nix).nixos
      ++ lib.optional (builtins.pathExists userModules) userModules;
  };
  publicOption =
    option:
    !option.internal && (lib.hasPrefix "confctl." option.name || lib.hasPrefix "cluster." option.name);
in
builtins.filter publicOption (lib.optionAttrSetToDocList evaluated.options)
